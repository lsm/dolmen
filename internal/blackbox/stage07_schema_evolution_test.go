package blackbox

import (
	"testing"
	"time"
)

func TestStage07SchemaEvolutionUnderTraffic(t *testing.T) {
	if app.daemon == nil {
		t.Fatal("the notification daemon from stage 5 is not connected")
	}

	drained := 0
	sawProbe := false
	deadline := time.Now().Add(5 * time.Second)
	quietFrom := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		f, ok := app.daemon.Poll()
		if !ok {
			if sawProbe && time.Now().After(quietFrom) {
				break
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if f.Event == "ready" {
			continue
		}
		if f.Event != "change" {
			t.Fatalf("daemon frame %q is not a change", f.Event)
		}
		change := decodeChange(t, f.Data)
		app.daemonLast = asStr(t, change["cursor"], "daemon cursor")
		drained++
		if asInt(t, change["row_id"], "daemon row_id") == app.probeID && asStr(t, change["table"], "daemon table") == "events" {
			sawProbe = true
		}
		quietFrom = time.Now().Add(300 * time.Millisecond)
	}
	if !sawProbe {
		t.Fatal("the daemon never delivered the stage 6 commit; the stream is not delivering")
	}

	migrated := op(t, "migrate", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "tickets",
		"changes": []any{
			map[string]any{
				"op":      "add_field",
				"field":   map[string]any{"name": "priority", "type": "number"},
				"default": 2,
			},
		},
	})
	table, _ := migrated["table"].(map[string]any)
	if table == nil {
		t.Fatalf("migrate response carries no table object: %v", migrated)
	}
	assertConforms(t, openapiSchema(t, "TableSchema"), table, "migrate.tickets")
	if version := asInt(t, table["version"], "migrated version"); version != 2 {
		t.Fatalf("tickets version after migrate: %d", version)
	}
	fields := fieldMap(t, describeTable(t, "tickets"))
	priority, ok := fields["priority"]
	if !ok {
		t.Fatal("describe_table does not report the migrated priority field")
	}
	if typ := asStr(t, priority["type"], "priority type"); typ != "number" {
		t.Fatalf("priority field type: %q", typ)
	}

	backfill := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT COUNT(*) AS n FROM tickets WHERE priority = 2",
	})
	rows := asMapList(t, backfill["rows"], "backfill rows")
	total := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT COUNT(*) AS n FROM tickets",
	})
	totalRows := asMapList(t, total["rows"], "ticket count rows")
	if asInt(t, rows[0]["n"], "backfilled count") != asInt(t, totalRows[0]["n"], "total count") {
		t.Fatalf("the priority backfill missed rows: %v of %v", rows[0], totalRows[0])
	}

	inserted := op(t, "insert", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "tickets",
		"records": []any{map[string]any{
			"subject": "Post-migration ticket rides the live stream",
			"body":    "filed after the schema changed under traffic",
			"status":  "open",
		}},
	})
	postID := asInt(t, inserted["ids"].([]any)[0], "post-migration ticket id")

	var postChange map[string]any
	for postChange == nil {
		change := app.daemon.NextChange(t, "post-migration delivery")
		if asStr(t, change["table"], "post-migration table") != "tickets" ||
			asStr(t, change["kind"], "post-migration kind") != "insert" ||
			asInt(t, change["row_id"], "post-migration row_id") != postID {
			continue
		}
		postChange = change
	}
	app.daemonLast = asStr(t, postChange["cursor"], "daemon cursor")

	postMigrate := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT priority FROM tickets WHERE subject = ?",
		"args":      []any{"Post-migration ticket rides the live stream"},
	})
	pmRows := asMapList(t, postMigrate["rows"], "post-migration rows")
	if len(pmRows) != 1 {
		t.Fatalf("post-migration ticket rows: %v", postMigrate)
	}
	if _, present := pmRows[0]["priority"]; !present || pmRows[0]["priority"] != nil {
		t.Fatalf("insert omitting the migrated optional field stored %v, expected NULL after the one-time backfill", pmRows[0]["priority"])
	}

	tail := op(t, "changes_since", map[string]any{
		"namespace": scenarioNamespace,
		"cursor":    app.daemonLast,
	})
	if changes, _ := tail["changes"].([]any); len(changes) != 0 {
		t.Fatalf("changes_since from the daemon tail replayed %v after delivering everything", changes)
	}
}
