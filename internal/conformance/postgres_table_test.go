package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type tableEngine interface {
	namespaceEngine
	CreateTable(context.Context, string, string, []schema.Field, store.TableOpts, [16]byte) (*schema.TableSchema, error)
	TableState(context.Context, string, string, []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error)
	ListTables(context.Context, string, []store.AuthBinding) ([]string, error)
	DescribeTable(context.Context, string, string, *store.RowScope, store.Incarnation) (*schema.TableSchema, int64, error)
	DropTable(context.Context, string, string, store.Incarnation) error
}

func TestTableBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var eng tableEngine
			if backend == "postgres" {
				eng = postgresNamespaceEngine(t).(tableEngine)
			} else {
				s, err := store.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				eng = s
				t.Cleanup(func() { s.Close() })
			}
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "project", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			for _, fields := range [][]schema.Field{
				{}, {{Name: "bad-name"}}, {{Name: "name", Type: schema.Boolean, Fulltext: true}},
				{{Name: "amount", Type: schema.Number, Default: "not a number"}},
				{{Name: "name", Required: true, Default: "invalid"}},
			} {
				if _, err := eng.CreateTable(ctx, "project", "rejected", fields, store.TableOpts{}, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("invalid fields accepted: %v", err)
				}
			}
			names, err := eng.ListTables(ctx, "project", nil)
			if err != nil || len(names) != 0 {
				t.Fatalf("failed DDL left a table: %v %v", names, err)
			}
			fields := []schema.Field{{Name: "body", Fulltext: true}, {Name: "count", Type: schema.Number, Default: json.Number("9007199254740993")}}
			table := strings.Repeat("a", 64)
			created, err := eng.CreateTable(ctx, "project", table, fields, store.TableOpts{}, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if created.Fields[0].Type != schema.String || created.Version != 1 {
				t.Fatalf("normalization: %+v", created)
			}
			if _, err := eng.CreateTable(ctx, "project", table, fields, store.TableOpts{}, [16]byte{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("duplicate: %v", err)
			}
			sc, inc, err := eng.TableState(ctx, "project", table, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Fields[1].Default != json.Number("9007199254740993") {
				t.Fatalf("default number changed: %#v", sc.Fields[1].Default)
			}
			if inc.Table != table || inc.Version != 1 || inc.DropGen != 0 {
				t.Fatalf("incarnation: %+v", inc)
			}
			_, count, err := eng.DescribeTable(ctx, "project", table, nil, store.Incarnation{})
			if err != nil || count != 0 {
				t.Fatalf("describe: %d %v", count, err)
			}
			if err := eng.DropTable(ctx, "project", table, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := eng.TableState(ctx, "project", table, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing table: %v", err)
			}
			if _, err := eng.CreateTable(ctx, "project", table, fields, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, next, err := eng.TableState(ctx, "project", table, nil)
			if err != nil || next.DropGen != inc.DropGen+1 {
				t.Fatalf("drop generation: %+v %v", next, err)
			}
		})
	}
}
