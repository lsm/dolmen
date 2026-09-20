package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const RegistryFile = "_grants.db"

const RootObject = "*"

const (
	SubjectPrincipal = "principal"
	SubjectGroup     = "group"
)

type Subject struct {
	Type string
	ID   string
}

func (s Subject) Valid() error {
	switch s.Type {
	case SubjectPrincipal, SubjectGroup:
	default:
		return fmt.Errorf("subject.type must be %q or %q, not %q", SubjectPrincipal, SubjectGroup, s.Type)
	}
	if s.Type == SubjectGroup {
		if !groupRe.MatchString(s.ID) {
			return fmt.Errorf("subject.id is not a usable group name: use 1 to 128 printable ASCII characters with no space or comma")
		}
		return nil
	}
	if !principalRe.MatchString(s.ID) {
		return fmt.Errorf("subject.id is not a usable principal: use 1 to 256 printable ASCII characters with no space")
	}
	if s.ID == AdminPrincipal {
		return fmt.Errorf("subject.id must not be %q: no identity source can yield the bootstrap principal, so a grant naming it could never be used", AdminPrincipal)
	}
	return nil
}

type Object struct {
	Namespace string
	Table     string
}

func (o Object) Root() bool { return o.Namespace == RootObject }

func (o Object) String() string {
	if o.Table == "" {
		return o.Namespace
	}
	return o.Namespace + "." + o.Table
}

type Grant struct {
	Subject   Subject
	Object    Object
	Verbs     VerbSet
	CreatedAt time.Time
}

type Registry struct {
	mu sync.Mutex
	db *sql.DB
}

func registryDSN(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("mode", "rwc")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_txlock", "immediate")
	u.RawQuery = q.Encode()
	return u.String()
}

func OpenRegistry(dir string) (*Registry, error) {
	db, err := sql.Open("sqlite", registryDSN(filepath.Join(dir, RegistryFile)))
	if err != nil {
		return nil, fmt.Errorf("open grant registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS grants (
		subject_type TEXT NOT NULL,
		subject_id   TEXT NOT NULL,
		namespace    TEXT NOT NULL,
		table_name   TEXT NOT NULL,
		verbs        INTEGER NOT NULL,
		created_at   TEXT NOT NULL,
		PRIMARY KEY (subject_type, subject_id, namespace, table_name)
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create grant registry: %w", err)
	}
	r := &Registry{db: db}
	if err := r.initKeys(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create key registry: %w", err)
	}
	if err := r.initKeyring(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create signing keyring: %w", err)
	}
	if err := r.initPending(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create sign-in state table: %w", err)
	}
	return r, nil
}

func (r *Registry) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func likePrefix(ns string) string {
	var b strings.Builder
	for _, r := range ns {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString("/%")
	return b.String()
}

func coveringObjects(o Object) []Object {
	out := []Object{{Namespace: RootObject}}
	if o.Root() {
		return out
	}
	segments := strings.Split(o.Namespace, "/")
	for i := range segments {
		out = append(out, Object{Namespace: strings.Join(segments[:i+1], "/")})
	}
	if o.Table != "" {
		out = append(out, o)
	}
	return out
}

func subjectsFor(id Identity) []Subject {
	out := []Subject{{Type: SubjectPrincipal, ID: id.Principal}}
	for _, g := range id.Groups {
		out = append(out, Subject{Type: SubjectGroup, ID: g})
	}
	return out
}

func (r *Registry) EffectiveVerbs(ctx context.Context, id Identity, obj Object) (VerbSet, error) {
	subjects := subjectsFor(id)
	objects := coveringObjects(obj)

	var args []any
	subjClauses := make([]string, 0, len(subjects))
	for _, s := range subjects {
		subjClauses = append(subjClauses, "(subject_type = ? AND subject_id = ?)")
		args = append(args, s.Type, s.ID)
	}
	objClauses := make([]string, 0, len(objects))
	for _, o := range objects {
		objClauses = append(objClauses, "(namespace = ? AND table_name = ?)")
		args = append(args, o.Namespace, o.Table)
	}

	query := "SELECT verbs FROM grants WHERE (" + strings.Join(subjClauses, " OR ") + ") AND (" + strings.Join(objClauses, " OR ") + ")"
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("resolve grants: %w", err)
	}
	defer rows.Close()

	var union VerbSet
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return 0, fmt.Errorf("resolve grants: %w", err)
		}
		union |= VerbSet(v)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("resolve grants: %w", err)
	}
	return union, nil
}

func (r *Registry) Grant(ctx context.Context, subj Subject, obj Object, verbs VerbSet) (Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, found, err := r.lookup(ctx, subj, obj)
	if err != nil {
		return Grant{}, err
	}
	if found {
		merged := existing.Verbs | verbs
		if merged != existing.Verbs {
			if _, err := r.db.ExecContext(ctx,
				"UPDATE grants SET verbs = ? WHERE subject_type = ? AND subject_id = ? AND namespace = ? AND table_name = ?",
				int64(merged), subj.Type, subj.ID, obj.Namespace, obj.Table); err != nil {
				return Grant{}, fmt.Errorf("merge grant: %w", err)
			}
			existing.Verbs = merged
		}
		return existing, nil
	}

	created := time.Now().UTC()
	if _, err := r.db.ExecContext(ctx,
		"INSERT INTO grants (subject_type, subject_id, namespace, table_name, verbs, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		subj.Type, subj.ID, obj.Namespace, obj.Table, int64(verbs), created.Format(time.RFC3339Nano)); err != nil {
		return Grant{}, fmt.Errorf("store grant: %w", err)
	}
	return Grant{Subject: subj, Object: obj, Verbs: verbs, CreatedAt: created}, nil
}

var ErrLastRootAdmin = errors.New("the deployment would be left with no usable root administrator")

func (r *Registry) otherRootAdminRemainsLocked(ctx context.Context, revoked Subject, headerReachable bool) (bool, error) {
	admins, err := r.rootAdminsLocked(ctx)
	if err != nil {
		return false, err
	}
	var remaining []Subject
	for _, a := range admins {
		if a != revoked {
			remaining = append(remaining, a)
		}
	}
	if len(remaining) == 0 {
		return false, nil
	}
	if headerReachable {
		for _, a := range remaining {
			if a.Type == SubjectPrincipal {
				return true, nil
			}
		}
	}
	keys, err := r.activeKeysLocked(ctx)
	if err != nil {
		return false, err
	}
	return rootReachableByKey(remaining, keys, ""), nil
}

func (r *Registry) Revoke(ctx context.Context, subj Subject, obj Object, verbs VerbSet, keepRootAdmin, headerReachable bool) (*Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, found, err := r.lookup(ctx, subj, obj)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	remaining := existing.Verbs &^ verbs
	if remaining == existing.Verbs {
		return &existing, nil
	}
	if keepRootAdmin && obj.Root() && existing.Verbs.Has(VerbAdmin) && !remaining.Has(VerbAdmin) {
		ok, err := r.otherRootAdminRemainsLocked(ctx, subj, headerReachable)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrLastRootAdmin
		}
	}
	if remaining == 0 {
		if _, err := r.db.ExecContext(ctx,
			"DELETE FROM grants WHERE subject_type = ? AND subject_id = ? AND namespace = ? AND table_name = ?",
			subj.Type, subj.ID, obj.Namespace, obj.Table); err != nil {
			return nil, fmt.Errorf("revoke grant: %w", err)
		}
		return nil, nil
	}
	if _, err := r.db.ExecContext(ctx,
		"UPDATE grants SET verbs = ? WHERE subject_type = ? AND subject_id = ? AND namespace = ? AND table_name = ?",
		int64(remaining), subj.Type, subj.ID, obj.Namespace, obj.Table); err != nil {
		return nil, fmt.Errorf("revoke grant: %w", err)
	}
	existing.Verbs = remaining
	return &existing, nil
}

func (r *Registry) lookup(ctx context.Context, subj Subject, obj Object) (Grant, bool, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT verbs, created_at FROM grants WHERE subject_type = ? AND subject_id = ? AND namespace = ? AND table_name = ?",
		subj.Type, subj.ID, obj.Namespace, obj.Table)
	var verbs int64
	var created string
	switch err := row.Scan(&verbs, &created); {
	case err == sql.ErrNoRows:
		return Grant{}, false, nil
	case err != nil:
		return Grant{}, false, fmt.Errorf("read grant: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Grant{}, false, fmt.Errorf("read grant: stored created_at %q is not a timestamp", created)
	}
	return Grant{Subject: subj, Object: obj, Verbs: VerbSet(verbs), CreatedAt: at}, true, nil
}

func (r *Registry) List(ctx context.Context, subj *Subject, obj *Object) ([]Grant, error) {
	query := "SELECT subject_type, subject_id, namespace, table_name, verbs, created_at FROM grants"
	var where []string
	var args []any
	if subj != nil {
		where = append(where, "subject_type = ? AND subject_id = ?")
		args = append(args, subj.Type, subj.ID)
	}
	if obj != nil && !obj.Root() {
		if obj.Table != "" {
			where = append(where, "namespace = ? AND table_name = ?")
			args = append(args, obj.Namespace, obj.Table)
		} else {
			where = append(where, `(namespace = ? OR namespace LIKE ? ESCAPE '\')`)
			args = append(args, obj.Namespace, likePrefix(obj.Namespace))
		}
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var g Grant
		var verbs int64
		var created string
		if err := rows.Scan(&g.Subject.Type, &g.Subject.ID, &g.Object.Namespace, &g.Object.Table, &verbs, &created); err != nil {
			return nil, fmt.Errorf("list grants: %w", err)
		}
		g.Verbs = VerbSet(verbs)
		if g.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("list grants: stored created_at %q is not a timestamp", created)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	sortGrants(out)
	return out, nil
}

func sortGrants(grants []Grant) {
	sort.SliceStable(grants, func(i, j int) bool {
		a, b := grants[i], grants[j]
		if a.Subject.Type != b.Subject.Type {
			return a.Subject.Type < b.Subject.Type
		}
		if a.Subject.ID != b.Subject.ID {
			return a.Subject.ID < b.Subject.ID
		}
		if a.Object.Namespace != b.Object.Namespace {
			return a.Object.Namespace < b.Object.Namespace
		}
		return a.Object.Table < b.Object.Table
	})
}

func (r *Registry) DropNamespace(ctx context.Context, ns string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM grants WHERE namespace = ? OR namespace LIKE ? ESCAPE '\'`, ns, likePrefix(ns)); err != nil {
		return fmt.Errorf("drop grants for namespace: %w", err)
	}
	return nil
}

func (r *Registry) DropTable(ctx context.Context, ns, table string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.db.ExecContext(ctx,
		"DELETE FROM grants WHERE namespace = ? AND table_name = ?", ns, table); err != nil {
		return fmt.Errorf("drop grants for table: %w", err)
	}
	return nil
}

func (r *Registry) NamespacesWithAnyGrant(ctx context.Context, id Identity) (map[string]struct{}, error) {
	subjects := subjectsFor(id)
	clauses := make([]string, 0, len(subjects))
	var args []any
	for _, s := range subjects {
		clauses = append(clauses, "(subject_type = ? AND subject_id = ?)")
		args = append(args, s.Type, s.ID)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT DISTINCT namespace FROM grants WHERE ("+strings.Join(clauses, " OR ")+")", args...)
	if err != nil {
		return nil, fmt.Errorf("list granted namespaces: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, fmt.Errorf("list granted namespaces: %w", err)
		}
		out[ns] = struct{}{}
	}
	return out, rows.Err()
}

func (r *Registry) RootAdmins(ctx context.Context) ([]Subject, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rootAdminsLocked(ctx)
}

func (r *Registry) rootAdminsLocked(ctx context.Context) ([]Subject, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT subject_type, subject_id, verbs FROM grants WHERE namespace = ? AND table_name = ''", RootObject)
	if err != nil {
		return nil, fmt.Errorf("read root administrators: %w", err)
	}
	defer rows.Close()
	var out []Subject
	for rows.Next() {
		var s Subject
		var verbs int64
		if err := rows.Scan(&s.Type, &s.ID, &verbs); err != nil {
			return nil, fmt.Errorf("read root administrators: %w", err)
		}
		if VerbSet(verbs).Has(VerbAdmin) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}
