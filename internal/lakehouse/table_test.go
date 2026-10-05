package lakehouse

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/apache/iceberg-go"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestTableSchemaAndNativeEvolutionPublishTogether(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "amount", Type: schema.Number, Default: json.Number("9223372036854775807")}, {Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, before, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	var oldID int
	if err := s.withNamespace(ctx, "app", func(n *namespace) error {
		state, err := loadTable(ctx, n, "app", "notes")
		if err != nil {
			return err
		}
		field, ok := state.native.Schema().FindFieldByName("amount")
		if !ok || !field.Type.Equals(iceberg.PrimitiveTypes.String) {
			t.Fatalf("number must preserve its canonical text: %v", field)
		}
		oldID = field.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changes := []schema.Change{{Op: schema.OpRenameField, From: "amount", To: "value"}, {Op: schema.OpAddField, Field: &schema.Field{Name: "required", Type: schema.Boolean, Required: true}, Default: true}}
	plan, err := s.PlanMigration(ctx, "app", "notes", changes, store.Embedder{}, before, nil, store.Incarnation{})
	if err != nil || !plan.DryRun {
		t.Fatalf("plan: %v %v", plan, err)
	}
	unchanged, guard, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil || guard != before || unchanged.Fields[0].Name != "amount" {
		t.Fatalf("planning published: %v %v", unchanged, err)
	}
	if _, err := s.Migrate(ctx, "app", "notes", changes, store.Embedder{}, before); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	if err := s.withNamespace(ctx, "app", func(n *namespace) error {
		state, err := loadTable(ctx, n, "app", "notes")
		if err != nil {
			return err
		}
		field, ok := state.native.Schema().FindFieldByName("value")
		if !ok || field.ID != oldID {
			t.Fatalf("rename lost stable Iceberg field ID: %v", field)
		}
		if _, ok := state.native.Schema().FindFieldByName("amount"); ok {
			t.Fatal("native schema retained old name")
		}
		if _, ok := state.native.Schema().FindFieldByName("required"); !ok {
			t.Fatal("native schema lacks required addition")
		}
		if state.schema.Version != 2 || state.schema.Fields[0].Default != json.Number("9223372036854775807") {
			t.Fatalf("logical schema lost exact default: %v", state.schema)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListMigrations(ctx, "app", "notes", store.Incarnation{})
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0].Changes, changes) {
		t.Fatalf("history: %v %v", history, err)
	}
}

func TestLakehouseCatalogUpgradesNamespaceFoundation(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.withNamespace(ctx, "app", func(n *namespace) error {
		for _, table := range []string{"_dolmen_lakehouse_tables", "_dolmen_lakehouse_ids", "_dolmen_lakehouse_commits", "_dolmen_lakehouse_changes", "_dolmen_lakehouse_idempotency", "_dolmen_lakehouse_counts"} {
			if _, err := n.db.ExecContext(ctx, `DROP TABLE `+table); err != nil {
				return err
			}
		}
		_, err := n.db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_meta SET format=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.withNamespace(ctx, "app", func(n *namespace) error {
		var format int
		err := n.db.QueryRowContext(ctx, `SELECT format FROM _dolmen_lakehouse_meta`).Scan(&format)
		if format != catalogFormat {
			t.Fatalf("format %d, want %d", format, catalogFormat)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "after upgrade"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("an upgraded catalog must accept appends: %v", err)
	}
}
