package lakehouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/catalog"
	"github.com/apache/iceberg-go/table"
	"github.com/google/uuid"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

const schemaProperty = "dolmen.schema"
const generationProperty = "dolmen.drop-generation"
const migrationsProperty = "dolmen.migrations"

func WithSecretKeyring(key *secret.Keyring) OpenOption { return func(s *Store) { s.secrets = key } }

func tableIdentifier(ns, name string) table.Identifier { return append(strings.Split(ns, "/"), name) }

func tableIdentifierNamespace(ns string) table.Identifier {
	return table.Identifier(strings.Split(ns, "/"))
}

func physicalType(f schema.Field) iceberg.Type {
	switch f.Type {
	case schema.Boolean:
		return iceberg.PrimitiveTypes.Bool
	case schema.Vector, schema.Secret:
		return iceberg.PrimitiveTypes.Binary
	default:
		return iceberg.PrimitiveTypes.String
	}
}

func nativeSchema(sc *schema.TableSchema) *iceberg.Schema {
	fields := []iceberg.NestedField{{ID: 1, Name: "id", Type: iceberg.PrimitiveTypes.Int64, Required: true}, {ID: 2, Name: "created_at", Type: iceberg.PrimitiveTypes.String, Required: true}}
	for _, f := range sc.Fields {
		fields = append(fields, iceberg.NestedField{ID: len(fields) + 1, Name: f.Name, Type: physicalType(f), Required: f.Required})
	}
	if sc.VectorizeField() != nil {
		fields = append(fields, iceberg.NestedField{ID: len(fields) + 1, Name: "_embedding", Type: iceberg.PrimitiveTypes.Binary})
	}
	if sc.HasOwner {
		fields = append(fields, iceberg.NestedField{ID: len(fields) + 1, Name: schema.OwnerColumn, Type: iceberg.PrimitiveTypes.String, Required: true})
	}
	return iceberg.NewSchema(0, fields...)
}

func decodeProperty(raw string, out any) error {
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: unreadable lakehouse schema registry: %v", store.ErrCatalogCorrupt, err)
	}
	return nil
}

type tableState struct {
	native      *table.Table
	schema      *schema.TableSchema
	incarnation store.Incarnation
}

func loadTable(ctx context.Context, n *namespace, ns, name string) (tableState, error) {
	var state tableState
	if err := schema.ValidateTableName(name); err != nil {
		return state, store.TableNotFound(ns, name)
	}
	native, err := n.catalog.LoadTable(ctx, tableIdentifier(ns, name))
	if errors.Is(err, catalog.ErrNoSuchTable) {
		return state, store.TableNotFound(ns, name)
	}
	if err != nil {
		return state, err
	}
	if err := decodeProperty(native.Properties()[schemaProperty], &state.schema); err != nil {
		return state, err
	}
	gen, err := strconv.ParseInt(native.Properties()[generationProperty], 10, 64)
	if err != nil || gen < 0 || state.schema == nil || state.schema.Name != name || state.schema.Namespace != ns || state.schema.Version < 1 {
		return state, fmt.Errorf("%w: invalid lakehouse table registry", store.ErrCatalogCorrupt)
	}
	state.native = native
	state.incarnation = store.Incarnation{NsGen: n.generation, Table: name, Version: int64(state.schema.Version), DropGen: gen}
	return state, nil
}

func checkExpected(state tableState, want store.Incarnation, version bool) error {
	if store.IncarnationIsZero(want) {
		return nil
	}
	got := state.incarnation
	if want.Table != "" && want.Table != got.Table {
		return store.ScopeTableMismatch(want.Table, got.Table)
	}
	if want.NsGen != [16]byte{} && want.NsGen != got.NsGen || (want.Table != "" || want.NsGen != [16]byte{} || want.DropGen != 0) && want.DropGen != got.DropGen {
		return store.ScopeIncarnationChanged(state.schema.Namespace, got.Table)
	}
	if version && want.Version != 0 && want.Version != got.Version {
		return &store.VersionConflictError{Namespace: state.schema.Namespace, Table: got.Table, ExpectedVersion: int(want.Version), CurrentVersion: int(got.Version)}
	}
	return nil
}

func (s *Store) CreateTable(ctx context.Context, ns, name string, fields []schema.Field, opts store.TableOpts, expected [16]byte) (*schema.TableSchema, error) {
	fields, err := store.ValidateTableDefinition(name, fields)
	if err != nil {
		return nil, err
	}
	if err := store.RequireSecretKey(s.secrets, fields); err != nil {
		return nil, err
	}
	if err := schema.ValidateRowAccess(opts.RowAccess); err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	if opts.RowAccess != "" {
		if err := store.ValidateOwnerCollision(fields); err != nil {
			return nil, err
		}
	}
	sc := &schema.TableSchema{Namespace: ns, Name: name, Version: 1, Fields: fields, RowAccess: opts.RowAccess, HasOwner: opts.RowAccess != ""}
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		if expected != [16]byte{} && expected != n.generation {
			return store.ScopeIncarnationChanged(ns, name)
		}
		exists, err := n.catalog.CheckTableExists(ctx, tableIdentifier(ns, name))
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: table %s.%s already exists", store.ErrInvalid, ns, name)
		}
		var gen int64
		err = n.db.QueryRowContext(ctx, `SELECT generation FROM _dolmen_lakehouse_tables WHERE name=?`, name).Scan(&gen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return err
		}
		directory := filepath.Join(n.dataDir, name+"-"+uuid.NewString())
		native, err := n.catalog.CreateTable(ctx, tableIdentifier(ns, name), nativeSchema(sc), catalog.WithLocation(fileLocation(directory)), catalog.WithProperties(iceberg.Properties{"format-version": "2", schemaProperty: string(raw), generationProperty: strconv.FormatInt(gen, 10), migrationsProperty: "[]"}))
		if err != nil {
			return err
		}
		return s.syncMetadata(native, n)
	})
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func (s *Store) TableState(ctx context.Context, ns, name string, auth []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error) {
	if len(auth) != 0 {
		return nil, store.Incarnation{}, derr.New(derr.Forbidden, "lakehouse authorization bindings are not implemented yet")
	}
	var state tableState
	err := s.withNamespace(ctx, ns, func(n *namespace) error { var err error; state, err = loadTable(ctx, n, ns, name); return err })
	return state.schema, state.incarnation, err
}

func (s *Store) DescribeTable(ctx context.Context, ns, name string, scope *store.RowScope, expected store.Incarnation) (*schema.TableSchema, int64, error) {
	var state tableState
	var count int64
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		var err error
		state, err = loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, expected, true); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
		}
		count, err = rowCount(ctx, n.db, state.incarnation, scope)
		return err
	})
	return state.schema, count, err
}

func (s *Store) ListTables(ctx context.Context, ns string, auth []store.AuthBinding) ([]string, error) {
	if len(auth) != 0 {
		return nil, derr.New(derr.Forbidden, "lakehouse authorization bindings are not implemented yet")
	}
	names := []string{}
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		for ident, err := range n.catalog.ListTables(ctx, table.Identifier(strings.Split(ns, "/"))) {
			if err != nil {
				return err
			}
			name := ident[len(ident)-1]
			if _, err := loadTable(ctx, n, ns, name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return nil
	})
	slices.Sort(names)
	return names, err
}

func (s *Store) DropTable(ctx context.Context, ns, name string, expected store.Incarnation) error {
	return s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, expected, true); err != nil {
			return err
		}
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, `DELETE FROM iceberg_tables WHERE catalog_name='dolmen' AND table_namespace=? AND table_name=? AND metadata_location=?`, strings.ReplaceAll(ns, "/", "."), name, state.native.MetadataLocation())
		if err != nil {
			return err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return store.ScopeIncarnationChanged(ns, name)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_tables(name,generation) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET generation=excluded.generation`, name, state.incarnation.DropGen+1); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_batches WHERE (owner, key) IN (SELECT owner, key FROM _dolmen_lakehouse_batch_tables WHERE table_name = ?)`, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_batch_tables WHERE table_name = ?`, name); err != nil {
			return err
		}
		for _, owned := range []string{"_dolmen_lakehouse_secrets", "_dolmen_lakehouse_idempotency", "_dolmen_lakehouse_counts", "_dolmen_lakehouse_ids"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+owned+` WHERE table_name = ? AND generation = ?`, name, state.incarnation.DropGen); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		location := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(state.native.Location(), "file://")))
		if rel, err := filepath.Rel(n.dataDir, location); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%w: Iceberg table location escaped its namespace", store.ErrCatalogCorrupt)
		}
		return os.RemoveAll(location)
	})
}

func (s *Store) ListMigrations(ctx context.Context, ns, name string, expected store.Incarnation) ([]store.Migration, error) {
	history := []store.Migration{}
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, expected, true); err != nil {
			return err
		}
		return decodeProperty(state.native.Properties()[migrationsProperty], &history)
	})
	return history, err
}

func (s *Store) syncMetadata(native *table.Table, n *namespace) error {
	path := filepath.FromSlash(strings.TrimPrefix(native.MetadataLocation(), "file://"))
	rel, err := filepath.Rel(n.dataDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: Iceberg metadata escaped its namespace", store.ErrCatalogCorrupt)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := publishCurrentMetadata(path); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		relative, err := filepath.Rel(s.dir, dir)
		if err != nil {
			return err
		}
		if err := s.syncDirectory(relative); err != nil {
			return err
		}
		if dir == n.dataDir {
			break
		}
	}
	return nil
}
