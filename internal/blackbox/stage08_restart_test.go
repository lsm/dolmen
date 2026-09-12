package blackbox

import (
	"strings"
	"testing"
)

func TestStage08RestartSurvivesEverything(t *testing.T) {
	pre := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql": `SELECT
			(SELECT COUNT(*) FROM kb_articles) AS kb,
			(SELECT COUNT(*) FROM tickets) AS tickets,
			(SELECT COUNT(*) FROM events) AS events`,
	})
	preRows := asMapList(t, pre["rows"], "pre-restart counts")
	if len(preRows) != 1 {
		t.Fatalf("pre-restart census: %v", pre)
	}
	if kb := asInt(t, preRows[0]["kb"], "kb count"); kb != 40 {
		t.Fatalf("kb_articles holds %d rows before restart", kb)
	}
	if tickets := asInt(t, preRows[0]["tickets"], "ticket count"); tickets < 10 {
		t.Fatalf("tickets holds %d rows before restart", tickets)
	}
	if events := asInt(t, preRows[0]["events"], "event count"); events < 4 {
		t.Fatalf("events holds %d rows before restart", events)
	}
	daemonCursor := app.daemonLast
	app.daemon.Close()

	old := app.srv
	if err := old.stop(); err != nil {
		if !strings.Contains(err.Error(), "signal") {
			t.Fatalf("SIGTERM did not end the server cleanly: %v", err)
		}
	}

	restarted, err := bootServer(old.dataDir, mainRetention, true)
	if err != nil {
		t.Fatalf("restart on the same data dir: %v", err)
	}
	app.srv = restarted
	app.openapi = nil
	app.schemas = nil

	post := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql": `SELECT
			(SELECT COUNT(*) FROM kb_articles) AS kb,
			(SELECT COUNT(*) FROM tickets) AS tickets,
			(SELECT COUNT(*) FROM events) AS events`,
	})
	postRows := asMapList(t, post["rows"], "post-restart counts")
	for _, key := range []string{"kb", "tickets", "events"} {
		if asInt(t, postRows[0][key], "post "+key) != asInt(t, preRows[0][key], "pre "+key) {
			t.Fatalf("%s did not survive the restart: %v -> %v", key, preRows[0], postRows[0])
		}
	}

	survivor := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT status, priority FROM tickets WHERE subject = ?",
		"args":      []any{"Customer asks for a refund after 40 days"},
	})
	survivorRows := asMapList(t, survivor["rows"], "surviving ticket")
	if len(survivorRows) != 1 {
		t.Fatalf("the filed ticket did not survive: %v", survivor)
	}
	if s := asStr(t, survivorRows[0]["status"], "surviving status"); s != "triaged" {
		t.Fatalf("ticket status after restart: %q", s)
	}
	if p := asInt(t, survivorRows[0]["priority"], "surviving priority"); p != 2 {
		t.Fatalf("ticket priority after restart: %d", p)
	}

	catchUp := op(t, "changes_since", map[string]any{
		"namespace": scenarioNamespace,
		"cursor":    daemonCursor,
	})
	if changes, _ := catchUp["changes"].([]any); len(changes) != 0 {
		t.Fatalf("changes landed while the server was down: %v", catchUp)
	}

	filed := op(t, "insert", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "tickets",
		"records": []any{map[string]any{
			"subject": "Filed after the restart",
			"body":    "proves the daemon cursor still replays new commits",
			"status":  "open",
		}},
	})
	newID := asInt(t, filed["ids"].([]any)[0], "post-restart ticket id")

	replay := op(t, "changes_since", map[string]any{
		"namespace": scenarioNamespace,
		"cursor":    daemonCursor,
	})
	changes, _ := replay["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes_since from the daemon cursor replayed %d changes, expected exactly the new commit: %v", len(changes), replay)
	}
	change, _ := changes[0].(map[string]any)
	if asInt(t, change["row_id"], "replayed row_id") != newID || asStr(t, change["table"], "replayed table") != "tickets" {
		t.Fatalf("the replay delivered %v, expected tickets row %d", change, newID)
	}
}
