package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

const RotateBatch = 200

type RotateOpts struct {
	Limit int
	Until time.Time
}

func (o RotateOpts) Stop(rotated int) bool {
	if o.Limit < 0 {
		return true
	}
	if o.Limit > 0 && rotated >= o.Limit {
		return true
	}
	return !o.Until.IsZero() && !time.Now().Before(o.Until)
}

func (o RotateOpts) Batch(rotated int) int {
	if o.Limit > 0 && o.Limit-rotated < RotateBatch {
		return o.Limit - rotated
	}
	return RotateBatch
}

type SecretTableRotation struct {
	Table     string
	Rotated   int64
	Remaining int64
	Keys      map[string]int64
}

type SecretRotation struct {
	Tables []SecretTableRotation
}

func (r SecretRotation) Remaining() int64 {
	var n int64
	for _, t := range r.Tables {
		n += t.Remaining
	}
	return n
}

func (r SecretRotation) Rotated() int64 {
	var n int64
	for _, t := range r.Tables {
		n += t.Rotated
	}
	return n
}

func RequireRotationKey(k *secret.Keyring) error {
	if k == nil {
		return RotationNeedsKey()
	}
	return nil
}

func RotationNeedsKey() error {
	return invalidf("rotate_secret_key needs a secret key: %s", secret.ErrNoKey)
}

func CheckRotationKeys(k *secret.Keyring, ns, table string, keys map[string]int64) error {
	configured := map[string]bool{}
	for _, id := range k.IDs() {
		configured[id] = true
	}
	var missing []string
	for id, n := range keys {
		if n > 0 && !configured[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return derr.New(derr.Conflict, "table %s.%s holds secret values encrypted under key id %s, which is not configured (configured: %s), so they cannot be re-encrypted and rotation stopped at this table; add that key to %s (or %s) and restart, then run rotate_secret_key again", ns, table, strings.Join(missing, ", "), strings.Join(k.IDs(), ", "), secret.EnvOldKeys, secret.EnvOldKeysFile)
}

func Reseal(k *secret.Keyring, col string, v any) ([]byte, error) {
	plain, err := OpenSecret(k, col, v)
	if err != nil {
		return nil, err
	}
	return k.Seal(plain)
}

func (s *Store) RotateSecrets(ctx context.Context, nsName string, opts RotateOpts) (SecretRotation, error) {
	if err := RequireRotationKey(s.secrets); err != nil {
		return SecretRotation{}, err
	}
	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return SecretRotation{}, err
	}
	defer n.unpin()
	tables, err := secretTables(ctx, n.rw)
	if err != nil {
		return SecretRotation{}, err
	}
	var out SecretRotation
	rotated := 0
	for _, sc := range tables {
		tr := SecretTableRotation{Table: sc.Name}
		before, err := s.secretKeyCounts(ctx, n, sc)
		if err != nil {
			return SecretRotation{}, err
		}
		if err := CheckRotationKeys(s.secrets, nsName, sc.Name, before); err != nil {
			return SecretRotation{}, err
		}
		for _, f := range sc.SecretFields() {
			for !opts.Stop(rotated) {
				done, err := s.rotateBatch(ctx, n, nsName, sc.Name, f.Name, opts.Batch(rotated))
				if err != nil {
					return SecretRotation{}, err
				}
				rotated += done
				tr.Rotated += int64(done)
				if done == 0 {
					break
				}
			}
		}
		if tr.Keys, err = s.secretKeyCounts(ctx, n, sc); err != nil {
			return SecretRotation{}, err
		}
		for id, c := range tr.Keys {
			if id != s.secrets.ID() {
				tr.Remaining += c
			}
		}
		out.Tables = append(out.Tables, tr)
	}
	return out, nil
}

func secretTables(ctx context.Context, db *sql.DB) ([]*schema.TableSchema, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, schema_json FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*schema.TableSchema
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var sc schema.TableSchema
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			return nil, fmt.Errorf("corrupt schema for table %s: %w", name, err)
		}
		if len(sc.SecretFields()) > 0 {
			sc.Name = name
			out = append(out, &sc)
		}
	}
	return out, rows.Err()
}

func (s *Store) secretKeyCounts(ctx context.Context, n *nsDB, sc *schema.TableSchema) (map[string]int64, error) {
	keys := map[string]int64{}
	for _, f := range sc.SecretFields() {
		rows, err := n.rw.QueryContext(ctx, fmt.Sprintf(`SELECT lower(hex(substr(%s, 2, 8))), count(*) FROM %s WHERE %s IS NOT NULL GROUP BY 1`, q(f.Name), q(sc.Name), q(f.Name)))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var c int64
			if err := rows.Scan(&id, &c); err != nil {
				rows.Close()
				return nil, err
			}
			keys[id] += c
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func (s *Store) rotateBatch(ctx context.Context, n *nsDB, nsName, table, col string, limit int) (int, error) {
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL AND substr(%s, 2, 8) != ? LIMIT ?`, q(col), q(table), q(col), q(col)), s.secrets.ActiveID(), limit)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id  int64
		old []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.old); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}
	for _, p := range batch {
		fresh, err := Reseal(s.secrets, col, p.old)
		if err != nil {
			if id, ok := secret.KeyID(p.old); ok {
				if cerr := CheckRotationKeys(s.secrets, nsName, table, map[string]int64{id: 1}); cerr != nil {
					return 0, cerr
				}
			}
			return 0, fmt.Errorf("rotate %s.%s row %d: %w", table, col, p.id, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ? AND %s = ?`, q(table), q(col), q(col)), fresh, p.id, p.old); err != nil {
			return 0, err
		}
	}
	return len(batch), tx.Commit()
}
