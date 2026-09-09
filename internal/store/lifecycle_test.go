package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func TestNamespaceListCreateDrop(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("fresh store must list no namespaces, got %v", nss)
	}

	if err := st.CreateNamespace("alpha"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "alpha.db")); err != nil {
		t.Fatalf("create must materialize the namespace file: %v", err)
	}
	if err := st.CreateNamespace("alpha"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate create must fail with ErrInvalid, got %v", err)
	}
	if err := st.CreateNamespace("../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name must fail with ErrInvalid, got %v", err)
	}
	if err := st.DropNamespace("../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("drop of invalid name must fail with ErrInvalid, got %v", err)
	}

	mustCreateNotes(t, st) // creates namespace "test" explicitly, with a WAL
	nss, err = st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 2 || nss[0] != "alpha" || nss[1] != "test" {
		t.Fatalf("expected [alpha test], got %v", nss)
	}

	if err := st.DropNamespace("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of missing namespace must 404, got %v", err)
	}

	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(st.dir, "test.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("test.db%s must be gone after drop", suffix)
		}
	}
	if _, ok := st.nss["test"]; ok {
		t.Fatal("drop must evict the cached connections")
	}

	nss, err = st.ListNamespaces()
	if err != nil {
		t.Fatalf("list after drop: %v", err)
	}
	if len(nss) != 1 || nss[0] != "alpha" {
		t.Fatalf("expected [alpha] after drop, got %v", nss)
	}

	// The name is reusable: recreated empty, none of the old tables.
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate dropped namespace: %v", err)
	}
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables on recreated namespace: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("recreated namespace must be empty, got %v", tables)
	}
}

// TestValidateNSPathGrammar pins §5.1: 1–3 segments of the v0.2.0 single-name
// grammar, no empty segments.
func TestValidateNSPathGrammar(t *testing.T) {
	valid := []string{
		"a",
		"a_b-c9",
		"a/b",
		"ab-/cd_ef",
		"a/b/c",
	}
	for _, ns := range valid {
		if err := validateNSPath(ns); err != nil {
			t.Errorf("validateNSPath(%q) = %v, want nil", ns, err)
		}
	}
	invalid := []string{
		"",
		"/a",
		"a/",
		"a//b",
		"a/b/",
		// Depth is capped at 3 namespace segments.
		"w/x/y/z",
		"w/x/y/z/v",
		"A/b",
		"a/B",
		"-a/b",
		"a/../b",
		"a/b./c",
		`a\b`,
		// A segment longer than the 64-char cap.
		strings.Repeat("x", 65),
		"a/" + strings.Repeat("y", 65),
	}
	for _, ns := range invalid {
		if err := validateNSPath(ns); !errors.Is(err, ErrInvalid) {
			t.Errorf("validateNSPath(%q) = %v, want ErrInvalid", ns, err)
		}
	}
}

// TestNestedNamespaceLayout pins §5.2: a/b/c is <data>/a/b/c.db, the parent
// directories appear on first child creation, a.db coexists with the a/
// subtree, and the cache keys by full path.
func TestNestedNamespaceLayout(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	// The child chain needs no depth-1 or depth-2 namespace to exist first:
	// creating a/b/c directly makes a/ and a/b/ on the way (§5.2, "created on
	// first child creation").
	if err := st.CreateNamespace("a/b/c"); err != nil {
		t.Fatalf("create a/b/c: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a", "b", "c.db")); err != nil {
		t.Fatalf("a/b/c must materialize at <data>/a/b/c.db: %v", err)
	}

	// A namespace and its subtree coexist: a.db beside the a/ directory.
	if err := st.CreateNamespace("a"); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a.db")); err != nil {
		t.Fatalf("depth-1 a must materialize at <data>/a.db, exactly v0.2.0's layout: %v", err)
	}
	if err := st.CreateNamespace("a/b"); err != nil {
		t.Fatalf("create a/b beside a.db: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a", "b.db")); err != nil {
		t.Fatalf("a/b must materialize at <data>/a/b.db: %v", err)
	}
	if err := st.CreateNamespace("a/b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate nested create must fail with ErrInvalid, got %v", err)
	}

	// The cache keys by full path: three namespaces, three databases, no
	// collision between a and its subtree even under one table name.
	for _, ns := range []string{"a", "a/b", "a/b/c"} {
		if _, err := st.CreateTable(ctx, ns, "notes", noteFields()); err != nil {
			t.Fatalf("create table in %s: %v", ns, err)
		}
		if _, err := st.Insert(ctx, ns, "notes", []map[string]any{{"title": "row in " + ns}}, testEmbed); err != nil {
			t.Fatalf("insert into %s: %v", ns, err)
		}
	}
	for _, ns := range []string{"a", "a/b", "a/b/c"} {
		if _, ok := st.nss[ns]; !ok {
			t.Fatalf("cache must hold a separate entry keyed by full path %q", ns)
		}
		rows, _, err := st.Query(ctx, ns, "SELECT title FROM notes", nil, 0, 0)
		if err != nil {
			t.Fatalf("query %s: %v", ns, err)
		}
		if len(rows) != 1 || rows[0]["title"] != "row in "+ns {
			t.Fatalf("namespace %s must see only its own row, got %v", ns, rows)
		}
	}

	// 3b's recursive listing reports the whole tree in full-path order:
	// a, a/b, a/b/c (§5.3).
	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 3 || nss[0] != "a" || nss[1] != "a/b" || nss[2] != "a/b/c" {
		t.Fatalf("recursive listing must report [a a/b a/b/c], got %v", nss)
	}
}

// TestListNamespacesRecursiveOrder pins §5.3's listing shape on a mixed
// tree: every namespace depth 1–3 appears exactly once, in lexicographic
// full-path order — which is NOT the walk's per-directory order (a/b/c
// sorts after a/b despite "b/" < "b.db" in ReadDir order, and a-x sorts
// before a/b because '-' < '/') — and the walk skips what the store would
// refuse to open: over-depth files and directories, invalid-segment
// directories, non-.db files, and symlinked names at any depth.
func TestListNamespacesRecursiveOrder(t *testing.T) {
	st := openStore(t)
	for _, ns := range []string{"a", "a-x", "a/b", "a/b/c", "ab", "z"} {
		mustNS(t, st, ns)
	}
	// Over-depth plants: a depth-4 database file (unreachable — its path
	// exceeds §5.1's cap, so the walk never enters a/b/c/) and an
	// invalid-segment directory holding a database. The over-depth
	// directory is also unreadable: the walk must not open it at all, so
	// an operator's junk directory beside a/b/c.db can neither fail nor
	// slow any listing — filtering after an eager ReadDir would fail here.
	if err := os.MkdirAll(filepath.Join(st.dir, "a", "b", "c"), 0o700); err != nil {
		t.Fatalf("plant depth-4 directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(st.dir, "a", "b", "c", "d.db"), nil, 0o600); err != nil {
		t.Fatalf("plant depth-4 file: %v", err)
	}
	deep := filepath.Join(st.dir, "a", "b", "c")
	if err := os.Chmod(deep, 0o000); err != nil {
		t.Fatalf("chmod the depth-4 directory unreadable: %v", err)
	}
	// Restore readability before TempDir's cleanup (registered earlier, so
	// it runs after this one) — RemoveAll must delete d.db inside.
	t.Cleanup(func() { os.Chmod(deep, 0o700) })
	if err := os.MkdirAll(filepath.Join(st.dir, "Bad Dir"), 0o700); err != nil {
		t.Fatalf("plant invalid-segment dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(st.dir, "Bad Dir", "x.db"), nil, 0o600); err != nil {
		t.Fatalf("plant database in invalid-segment dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(st.dir, "plain.txt"), nil, 0o600); err != nil {
		t.Fatalf("plant non-database file: %v", err)
	}
	// A symlinked directory is not descended (IsDir is lstat semantics), so
	// a database planted behind it — outside the data directory — never
	// surfaces; a symlinked database beside real ones is not listed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "leak.db"), nil, 0o600); err != nil {
		t.Fatalf("plant external database: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(st.dir, "linkdir")); err != nil {
		t.Fatalf("plant directory symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "leak.db"), filepath.Join(st.dir, "a", "leak.db")); err != nil {
		t.Fatalf("plant file symlink: %v", err)
	}

	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"a", "a-x", "a/b", "a/b/c", "ab", "z"}
	if !reflect.DeepEqual(nss, want) {
		t.Fatalf("recursive listing must be %v in full-path order, got %v", want, nss)
	}
}

// TestListNamespacesPrefix pins §5.3's optional prefix: the listing becomes
// the prefix's recursive subtree, the prefix itself included; a prefix that
// names no namespace still lists its descendants; an absent or invalid
// prefix lists nothing / fails, and a sibling sharing only a stem ("ab"
// under prefix "a") stays out.
func TestListNamespacesPrefix(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	for _, ns := range []string{"a", "a/b", "a/b/c", "ab", "other"} {
		mustNS(t, st, ns)
	}

	list := func(prefix string) []string {
		t.Helper()
		nss, err := st.Store.ListNamespaces(ctx, prefix, nil)
		if err != nil {
			t.Fatalf("list prefix %q: %v", prefix, err)
		}
		return nss
	}
	if got := list(""); !reflect.DeepEqual(got, []string{"a", "a/b", "a/b/c", "ab", "other"}) {
		t.Fatalf("empty prefix lists everything in order, got %v", got)
	}
	if got := list("a"); !reflect.DeepEqual(got, []string{"a", "a/b", "a/b/c"}) {
		t.Fatalf("prefix a lists its subtree including itself, not the ab sibling, got %v", got)
	}
	if got := list("a/b"); !reflect.DeepEqual(got, []string{"a/b", "a/b/c"}) {
		t.Fatalf("prefix a/b lists [a/b a/b/c], got %v", got)
	}
	if got := list("a/b/c"); !reflect.DeepEqual(got, []string{"a/b/c"}) {
		t.Fatalf("prefix a/b/c lists [a/b/c], got %v", got)
	}
	if got := list("other"); !reflect.DeepEqual(got, []string{"other"}) {
		t.Fatalf("prefix other lists [other], got %v", got)
	}
	if got := list("nope"); len(got) != 0 {
		t.Fatalf("absent prefix lists nothing, got %v", got)
	}

	// A prefix that is not itself a namespace still lists its descendants:
	// the subtree is a path filter, not an existence claim.
	st2 := openStore(t)
	mustNS(t, st2, "p/q")
	if got, err := st2.Store.ListNamespaces(ctx, "p", nil); err != nil || !reflect.DeepEqual(got, []string{"p/q"}) {
		t.Fatalf("prefix p over a child-only tree must list [p/q], got %v (%v)", got, err)
	}

	// The prefix is held to the same §5.1 grammar as any namespace path.
	for _, prefix := range []string{"A", "/a", "a/", "a//b", "a/b/c/d", "a/b/c/d/e"} {
		if _, err := st.Store.ListNamespaces(ctx, prefix, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("ListNamespaces(prefix %q) = %v, want ErrInvalid", prefix, err)
		}
	}

	// The subtree walk starts inside the prefix's path and never touches a
	// sibling: an unreadable directory elsewhere under the data directory —
	// an operator's staging area — can neither fail nor slow an unrelated
	// subtree listing, and the leaf-only drop guard, which counts
	// descendants through the same listing, inherits the isolation.
	st3 := openStore(t)
	mustNS(t, st3, "a/b")
	locked := filepath.Join(st3.dir, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("plant locked sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "junk.db"), nil, 0o600); err != nil {
		t.Fatalf("plant junk database: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod locked sibling: %v", err)
	}
	// Restore readability before TempDir's cleanup (registered earlier, so
	// it runs after this one) — RemoveAll must delete the junk inside.
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if got, err := st3.Store.ListNamespaces(ctx, "a", nil); err != nil || !reflect.DeepEqual(got, []string{"a/b"}) {
		t.Fatalf("subtree listing must not touch the unreadable sibling, got %v (%v)", got, err)
	}
	if got, err := st3.Store.ListNamespaces(ctx, "a/b", nil); err != nil || !reflect.DeepEqual(got, []string{"a/b"}) {
		t.Fatalf("leaf listing beside the unreadable sibling, got %v (%v)", got, err)
	}

	// A symlinked component is followed by nothing: the subtree through it
	// lists empty, never the outside directory's contents.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "leak.db"), nil, 0o600); err != nil {
		t.Fatalf("plant external database: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(st3.dir, "sym")); err != nil {
		t.Fatalf("plant symlinked component: %v", err)
	}
	if got, err := st3.Store.ListNamespaces(ctx, "sym", nil); err != nil || len(got) != 0 {
		t.Fatalf("a symlinked subtree component must list empty, got %v (%v)", got, err)
	}
	if got, err := st3.Store.ListNamespaces(ctx, "sym/deep", nil); err != nil || len(got) != 0 {
		t.Fatalf("a path through a symlinked component must list empty, got %v (%v)", got, err)
	}
}

// TestDropNamespaceRejectsDescendants pins §5.4's leaf-only drop: a
// namespace with descendants is refused — ErrInvalid naming the descendant
// count — before anything is evicted or deleted; the children go first
// (child-first drops succeed down to the leaf), a leftover empty directory
// from an already-dropped child blocks nothing, and a name whose file is
// gone reports not_found even when descendants exist.
func TestDropNamespaceRejectsDescendants(t *testing.T) {
	st := openStore(t)
	for _, ns := range []string{"a", "a/b", "a/b/c"} {
		mustNS(t, st, ns)
	}

	err := st.DropNamespace("a")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("dropping a parent must fail with ErrInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "2 descendant namespaces") {
		t.Fatalf("the refusal must name the descendant count, got %q", err.Error())
	}
	err = st.DropNamespace("a/b")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "1 descendant namespace") {
		t.Fatalf("dropping a/b must fail naming its 1 descendant, got %v", err)
	}
	// The refused drops evicted and deleted nothing: the tree is intact
	// (the listing — which opens nothing — would also catch a deleted
	// file, and the cache would catch an evicted entry).
	nss, lerr := st.ListNamespaces()
	if lerr != nil || !reflect.DeepEqual(nss, []string{"a", "a/b", "a/b/c"}) {
		t.Fatalf("refused drops must leave the tree intact, got %v (%v)", nss, lerr)
	}
	if _, ok := st.nss["a"]; !ok {
		t.Fatal("refused drop must not evict the namespace's cached connections")
	}

	// Child-first: the leaf drops, then its parent (the now-empty a/
	// directory left by a/b's drop is not a descendant), then the root.
	for _, ns := range []string{"a/b/c", "a/b", "a"} {
		if err := st.DropNamespace(ns); err != nil {
			t.Fatalf("child-first drop of %s: %v", ns, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a")); err != nil {
		t.Fatalf("the empty a/ directory must survive (drops never remove directories): %v", err)
	}

	// A name with descendants but no file of its own is not a namespace:
	// not_found, not the descendant refusal.
	mustNS(t, st, "x/y")
	if err = st.DropNamespace("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of a file-less name must 404, got %v", err)
	}
}
// TestNestedNamespaceOpenNeverCreates pins the §6.2 global rule at depth > 1:
// opening a missing namespace is ErrNotFound and leaves no directory behind,
// and every entry point rejects invalid paths before touching the filesystem.
func TestNestedNamespaceOpenNeverCreates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.ListTables(ctx, "ghost/child"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opening a missing child must 404, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "ghost")); !os.IsNotExist(err) {
		t.Fatalf("opening a missing namespace must not create its parent directory, stat err = %v", err)
	}

	for _, ns := range []string{"a/b/c/d", "/a", "a//b", "A/b", ""} {
		if err := st.CreateNamespace(ns); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateNamespace(%q) = %v, want ErrInvalid", ns, err)
		}
		if err := st.DropNamespace(ns); !errors.Is(err, ErrInvalid) {
			t.Errorf("DropNamespace(%q) = %v, want ErrInvalid", ns, err)
		}
		if _, err := st.ListTables(ctx, ns); !errors.Is(err, ErrInvalid) {
			t.Errorf("ListTables(%q) = %v, want ErrInvalid", ns, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("rejected paths must not create directories, stat err = %v", err)
	}
}

// TestDropNestedNamespace pins the drop at depth > 1: the nested .db and its
// WAL sidecars go, the parent namespace and the directory survive, and the
// child name is reusable.
func TestDropNestedNamespace(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustNS(t, st, "a")
	mustNS(t, st, "a/b")
	if _, err := st.CreateTable(ctx, "a/b", "notes", noteFields()); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := st.Insert(ctx, "a/b", "notes", []map[string]any{{"title": "wal row"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := st.DropNamespace("a/b"); err != nil {
		t.Fatalf("drop nested: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(st.dir, "a", "b.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("a/b.db%s must be gone after drop, stat err = %v", suffix, err)
		}
	}
	if _, ok := st.nss["a/b"]; ok {
		t.Fatal("drop must evict the nested namespace's cached connections")
	}
	// v0.2.0 semantics never remove directories, and the parent namespace is
	// untouched by its child's drop.
	if _, err := os.Stat(filepath.Join(st.dir, "a")); err != nil {
		t.Fatalf("the a/ directory must survive its child's drop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, "a.db")); err != nil {
		t.Fatalf("the parent namespace's own database must survive: %v", err)
	}

	// The child name is reusable and starts empty.
	if err := st.CreateNamespace("a/b"); err != nil {
		t.Fatalf("recreate dropped child: %v", err)
	}
	tables, err := st.ListTables(ctx, "a/b")
	if err != nil {
		t.Fatalf("list tables on recreated child: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("recreated child must be empty, got %v", tables)
	}
}

// TestNamespacePathSymlinkContainment pins the physical half of containment:
// the segment grammar bars "." and "/" lexically, verifyNSDirs and the
// regular-file check bar a planted symlink from steering namespace I/O out
// of the data directory — creating, opening, and dropping all refuse.
func TestNamespacePathSymlinkContainment(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	outside := t.TempDir()

	// A planted intermediate symlink: <data>/a -> outside.
	if err := os.Symlink(outside, filepath.Join(st.dir, "a")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}
	if err := st.CreateNamespace("a/b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("create through a symlinked parent must fail with ErrInvalid, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "b.db")); !os.IsNotExist(err) {
		t.Fatalf("the refused create must not write outside the data directory, stat err = %v", err)
	}

	// Opening and dropping an external file reachable through the symlink
	// are refused too — with the file present, so refusal is observable as
	// the file's survival, not just a 404.
	external := filepath.Join(outside, "b.db")
	if err := os.WriteFile(external, nil, 0o600); err != nil {
		t.Fatalf("plant external file: %v", err)
	}
	if _, err := st.ListTables(ctx, "a/b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("open through a symlinked parent must fail with ErrInvalid, got %v", err)
	}
	if err := st.DropNamespace("a/b"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("drop through a symlinked parent must fail with ErrInvalid, got %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("the refused drop must not remove the external file: %v", err)
	}

	// The namespace's own name is held to the same rule: a symlink named
	// like a namespace database is not a namespace.
	if err := os.Symlink(external, filepath.Join(st.dir, "link.db")); err != nil {
		t.Fatalf("plant file symlink: %v", err)
	}
	if _, err := st.ListTables(ctx, "link"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("opening a symlinked namespace file must fail with ErrInvalid, got %v", err)
	}
	if err := st.DropNamespace("link"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dropping a symlinked namespace file must fail with ErrInvalid, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(st.dir, "link.db")); err != nil {
		t.Fatalf("the refused drop must leave the symlink itself alone: %v", err)
	}

	// Listing applies the same rule: a symlink named like a namespace database
	// has a valid stem and is not a directory, but it must not be listed — a
	// listed namespace must be one the store can open (this store's only
	// entries are the two planted symlinks, so the list is empty).
	nss, err := st.ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("listing must exclude entries the store would refuse to open, got %v", nss)
	}
}

func TestDropNamespaceSurvivesRestartWithWAL(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ls := legacy(st)
	mustCreateNotes(t, ls)
	if _, err := ls.Insert(ctx, "test", "notes", []map[string]any{{"title": "wal row"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err = Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := legacy(st).DropNamespace("test"); err != nil {
		t.Fatalf("drop after restart: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, "test.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("test.db%s must be gone after drop with a WAL present", suffix)
		}
	}
	nss, err := legacy(st).ListNamespaces()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nss) != 0 {
		t.Fatalf("expected no namespaces after drop, got %v", nss)
	}
}

func TestDropNamespaceWithConcurrentWriters(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Errors are expected once the drop lands (the table, then the
				// namespace, vanish underneath the writers); what must not
				// happen is a deadlock, a panic, or the drop failing.
				_, _ = st.Insert(ctx, "test", "notes", []map[string]any{{"title": "racing"}}, testEmbed)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the writers engage
	if err := st.DropNamespace("test"); err != nil {
		t.Fatalf("drop under concurrency must succeed, got %v", err)
	}
	close(stop)
	wg.Wait()

	// The dropped name must be reusable and the old table must not survive.
	// 2b retired implicit recreation (nothing recreates the name underneath
	// the writers anymore — 2c's ensureNamespace restores that at the op
	// layer), so recreate explicitly and require the fresh namespace to be
	// fully usable. A straggler connection from the evicted pools closing
	// after the namespace's recreation deletes its WAL sidecars by path,
	// leaving the new pools poisoned (read-only opens then fail with disk I/O
	// errors) — evict drains precisely to prevent that.
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate dropped namespace: %v", err)
	}
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("post-drop state must be queryable: %v", err)
	}
	for _, tb := range tables {
		if tb == "notes" {
			t.Fatal("dropped table must not survive a concurrent recreate")
		}
	}
	if _, err := st.CreateTable(ctx, "test", "fresh", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("post-drop namespace must accept writes: %v", err)
	}
}

func TestDropTable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String}},
	}, testEmbed, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, _, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "once"}}, testEmbed, "op-1"); err != nil {
		t.Fatalf("idempotent insert: %v", err)
	}

	if err := st.DropTable(ctx, "test", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of missing table must 404, got %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("expected no tables after drop, got %v", tables)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	count := func(sql string) int64 {
		t.Helper()
		var c int64
		if err := n.ro.QueryRowContext(ctx, sql).Scan(&c); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return c
	}
	if c := count(`SELECT count(*) FROM sqlite_master WHERE name IN ('notes', 'notes__fts')`); c != 0 {
		t.Fatalf("table and FTS shadow must be gone from sqlite_master, got %d rows", c)
	}
	if c := count(`SELECT count(*) FROM _dolmen_tables WHERE name = 'notes'`); c != 0 {
		t.Fatal("registry row must be gone")
	}
	if c := count(`SELECT count(*) FROM _dolmen_migrations WHERE table_name = 'notes'`); c != 0 {
		t.Fatal("migration history must be gone")
	}
	if c := count(`SELECT count(*) FROM _dolmen_idempotency WHERE table_name = 'notes'`); c != 0 {
		t.Fatal("idempotency keys must be gone")
	}
	if c := count(`SELECT count(*) FROM _dolmen_drop_gen WHERE table_name = 'notes' AND gen = 1`); c != 1 {
		t.Fatal("drop generation must be persisted at 1")
	}

	// Recreating the name starts fresh: version 1, and the dropped table's
	// idempotency key does not replay the old ids.
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe recreated table: %v", err)
	}
	if sc.Version != 1 {
		t.Fatalf("recreated table must be version 1, got %d", sc.Version)
	}
	if sc.Field("extra") != nil {
		t.Fatal("recreated table must not inherit dropped-table fields")
	}
	_, replayed, err := st.InsertIdempotent(ctx, "test", "notes",
		[]map[string]any{{"title": "fresh"}}, testEmbed, "op-1")
	if err != nil {
		t.Fatalf("insert with the old key on the recreated table: %v", err)
	}
	if replayed {
		t.Fatal("a recreated table must not replay the dropped table's idempotency keys")
	}
}

func TestDropTableRemovesSearch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "findme"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "findme", 0, 10, false, "", nil); err != nil {
		t.Fatalf("search before drop: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "findme", 0, 10, false, "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("search on dropped table must 404, got %v", err)
	}
}

// pausingEmbedder returns an embedder whose first call blocks until released
// (letting a test drop + recreate the table mid-embed), then behaves like
// fakeEmbed for this and all later calls — as the insert retry loop requires.
func pausingEmbedder() (Embedder, func(), func()) {
	paused := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	return Embedder{
		Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			once.Do(func() { close(paused); <-release })
			return fakeEmbed(ctx, texts)
		},
		Identity: "fake-space",
	}, func() { <-paused }, func() { close(release) }
}

// recreatedFields is noteFields with score flipped from number to boolean: a
// recreate under the same name and version that a stale write must not accept
// (0.9 coerces cleanly against the old number field, not the new boolean one).
func recreatedFields() []schema.Field {
	f := noteFields()
	for i := range f {
		if f[i].Name == "score" {
			f[i].Type = schema.Boolean
		}
	}
	return f
}

func TestDropTableDuringInsertEmbedPause(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	emb, waitPaused, release := pausingEmbedder()
	type outcome struct {
		ids []int64
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ids, err := st.Insert(ctx, "test", "notes", []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- outcome{ids, err}
	}()

	// The insert has read the schema and is mid-embed: drop and recreate the
	// table under the same name, same version, different field types.
	waitPaused()
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	release()

	// The stale attempt must be discarded, not committed: the retry
	// re-validates against the recreated table and rejects the record.
	out := <-done
	if out.err == nil {
		t.Fatalf("stale insert must not commit into the recreated table, got ids %v", out.ids)
	}
	if !errors.Is(out.err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid from re-validation against the recreated table, got %v", out.err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if f := sc.Field("score"); f == nil || f.Type != schema.Boolean {
		t.Fatalf("recreated table must keep its own schema, got %v", sc.Fields)
	}
}

func TestDropTableDuringUpsertByKeyEmbedPause(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	emb, waitPaused, release := pausingEmbedder()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"}, []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- err
	}()

	waitPaused()
	if err := st.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	release()

	if err := <-done; err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale upsert must re-validate and fail against the recreated table, got %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
}

// The drop generation is persisted, so the guard also holds when a second
// Store instance (or a second server process) sharing the data directory
// performs the drop + recreate while this instance's write is mid-embedding.
func TestDropTableBySecondStoreInstanceDuringEmbedPause(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	stA, err := Open(dir)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { stA.Close() })
	stB, err := Open(dir)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	t.Cleanup(func() { stB.Close() })
	lsA, lsB := legacy(stA), legacy(stB)
	mustNS(t, lsA, "test")
	if _, err := lsA.CreateTable(ctx, "test", "notes", noteFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	emb, waitPaused, release := pausingEmbedder()
	done := make(chan error, 1)
	go func() {
		_, err := lsA.Insert(ctx, "test", "notes", []map[string]any{
			{"title": "stale", "body": "validated against the dead schema", "score": 0.9},
		}, emb)
		done <- err
	}()

	waitPaused()
	if err := lsB.DropTable(ctx, "test", "notes"); err != nil {
		t.Fatalf("drop via B: %v", err)
	}
	if _, err := lsB.CreateTable(ctx, "test", "notes", recreatedFields()); err != nil {
		t.Fatalf("recreate via B: %v", err)
	}
	release()

	if err := <-done; err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale insert on A must re-validate and fail against B's recreated table, got %v", err)
	}
	rows, _, err := lsB.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil, 0, 0)
	if err != nil {
		t.Fatalf("count via B: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("recreated table must stay empty, got %v", rows)
	}
}

func TestCreateNamespaceConcurrentReservation(t *testing.T) {
	st := openStore(t)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.CreateNamespace("race")
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrInvalid):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("exactly one creator must win the reservation, got %d", won)
	}
}

func TestNamespaceFileRemovedOutOfBand(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st) // namespace "test" is open and cached

	// Simulate an operator deleting the file under a live server.
	if err := os.Remove(filepath.Join(st.dir, "test.db")); err != nil {
		t.Fatalf("out-of-band remove: %v", err)
	}
	if err := st.DropNamespace("test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop of the removed namespace must 404, got %v", err)
	}
	if _, ok := st.nss["test"]; ok {
		t.Fatal("stale cache entry must be evicted (pools closed, not orphaned)")
	}

	// The name is creatable again and starts fresh.
	if err := st.CreateNamespace("test"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	tables, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("recreated namespace must be empty, got %v", tables)
	}
}

// The drop must not complete while a transaction holds one of the
// namespace's connections: the paths stay reserved until the straggler's
// final close, so its WAL-sidecar unlink cannot hit the next incarnation.
// Update on a vectorized field re-embeds inside the write transaction, so a
// pausing embedder pins the namespace's only rw connection deterministically.
func TestDropNamespaceWaitsForInFlightTransaction(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	// The update below must match a row: with none, it skips embedding
	// entirely and nothing holds the connection.
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "seed"}}, testEmbed); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	emb, waitPaused, release := pausingEmbedder()
	updated := make(chan error, 1)
	go func() {
		_, err := st.Update(ctx, "test", "notes", "id > 0", nil, map[string]any{"body": "held open"}, emb)
		updated <- err
	}()
	waitPaused() // the update now holds the rw connection inside its tx

	dropped := make(chan error, 1)
	go func() { dropped <- st.DropNamespace("test") }()
	select {
	case err := <-dropped:
		t.Fatalf("drop must wait for the in-flight transaction, returned early with %v", err)
	case <-time.After(150 * time.Millisecond):
		// still waiting, as it must
	}

	release()
	if err := <-updated; err != nil {
		t.Fatalf("in-flight update must complete before the drop: %v", err)
	}
	if err := <-dropped; err != nil {
		t.Fatalf("drop: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(st.dir, "test.db"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("test.db%s must be gone after the waited drop", suffix)
		}
	}
}
