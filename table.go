package dolmen

import (
	"context"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) CreateTable(ctx context.Context, namespace, table string, fields []Field) (r0 TableSchema, err error) {
	ctx, span := s.startOp(ctx, "create_table", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return TableSchema{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return TableSchema{}, facadeErr(err)
	}
	for _, f := range fields {
		if !f.Vectorize {
			continue
		}
		if !s.providerUsable() {
			if err := schema.Validate(schema.Normalize(fieldsToSchema(fields))); err != nil {
				return TableSchema{}, derr.New(derr.InvalidRequest, "%s", err)
			}
			return TableSchema{}, derr.New(derr.InvalidRequest, "field %q has vectorize, but this store has no usable embedding provider (none was supplied with WithEmbedding, or the configured one does not report its identity); the table is not created — create the field without vectorize, or open the store with a provider", f.Name)
		}
		break
	}
	ns := ops.NormalizeNamespace(namespace)
	if err := ops.EnsureNamespace(ctx, s.eng, ns); err != nil {
		return TableSchema{}, facadeErr(err)
	}
	sc, err := s.eng.CreateTable(ctx, ns, ops.NormalizeTable(table), fieldsToSchema(fields), store.TableOpts{}, [16]byte{})
	if err != nil {
		return TableSchema{}, facadeErr(err)
	}
	return schemaToTableSchema(sc), nil
}

func (s *Store) providerUsable() bool {
	return s.emb != nil && s.emb.Identity() != ""
}

func (s *Store) ListTables(ctx context.Context, namespace string) (r0 []string, err error) {
	ctx, span := s.startOp(ctx, "list_tables", namespace, "")
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return nil, facadeErr(err)
	}
	ns := ops.NormalizeNamespace(namespace)
	tables, err := s.eng.ListTables(ctx, ns, nil)
	if err != nil {
		return nil, facadeErr(err)
	}
	if tables == nil {
		tables = []string{}
	}
	return tables, nil
}

func (s *Store) DescribeTable(ctx context.Context, namespace, table string) (r0 TableSchema, r1 int64, err error) {
	ctx, span := s.startOp(ctx, "describe_table", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return TableSchema{}, 0, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return TableSchema{}, 0, facadeErr(err)
	}
	if tbl := ops.NormalizeTable(table); tbl == "" || !validTableName(tbl) {
		return TableSchema{}, 0, derr.New(derr.InvalidRequest, "table must match ^[a-z][a-z0-9_]{0,63}$ and not contain __fts or start with sqlite_")
	}
	ns := ops.NormalizeNamespace(namespace)
	sc, count, err := s.eng.DescribeTable(ctx, ns, ops.NormalizeTable(table), nil, store.Incarnation{})
	if err != nil {
		return TableSchema{}, 0, facadeErr(err)
	}
	return schemaToTableSchema(sc), count, nil
}

func (s *Store) DropTable(ctx context.Context, namespace, table string) (err error) {
	ctx, span := s.startOp(ctx, "drop_table", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return facadeErr(err)
	}
	if tbl := ops.NormalizeTable(table); tbl == "" || !validTableName(tbl) {
		return derr.New(derr.InvalidRequest, "table must match ^[a-z][a-z0-9_]{0,63}$ and not contain __fts or start with sqlite_")
	}
	ns := ops.NormalizeNamespace(namespace)
	return facadeErr(s.eng.DropTable(ctx, ns, ops.NormalizeTable(table), store.Incarnation{}))
}
