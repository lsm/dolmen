package blackbox

import (
	"strings"
	"testing"
)

type kbArticle struct {
	title string
	body  string
}

var kbCorpus = []kbArticle{
	{"Refund policy", "We refund any purchase within 30 days of the charge, no questions asked"},
	{"Refund exceptions", "A refund is not available for custom work already delivered"},
	{"Refund timing", "Expect the refund to reach your card within five business days"},
	{"Cancel your subscription", "To cancel, open Settings, choose Plan, and press cancel subscription"},
	{"Cancel and rebook", "You may cancel a booking and rebook at any time before the event starts"},
	{"Cancellation notice", "Send a cancellation email to support and we process it the same day"},
	{"Reset your password", "Open the login page and choose forgot password to reset your credentials"},
	{"Two factor login", "Enable two factor authentication from Profile after you login"},
	{"Login sessions", "Each login session expires after 30 days of inactivity"},
	{"Billing invoice download", "Every invoice is available for download from the billing history page"},
	{"Update your card", "Replace an expired card from the billing section before the next charge"},
	{"Billing address", "Correct your billing address so tax is calculated for the right region"},
	{"API keys", "Create an API key from Settings, Developers, and keep it secret"},
	{"API rate limits", "The API allows 600 requests per minute per key"},
	{"API webhooks", "Register a webhook endpoint to receive event callbacks over the API"},
	{"Invite a teammate", "Add a seat by inviting a teammate from the Team page"},
	{"Remove a seat", "Removing a seat frees it for reassignment at the next billing cycle"},
	{"Team roles", "Assign the admin or viewer role to each seat on the team"},
	{"Upgrade your plan", "Move to a higher tier any time; we prorate the upgrade"},
	{"Downgrade your plan", "Downgrade takes effect at the end of the current period"},
	{"Compare plans", "Each tier differs in seats, API volume, and support level"},
	{"Export your data", "Request an export archive and download it when ready"},
	{"Data retention", "We keep deleted records for 30 days before purging them"},
	{"GDPR requests", "Email privacy to exercise your GDPR rights over personal data"},
	{"Report a bug", "Attach reproduction steps when you report a bug so we can triage fast"},
	{"Status page", "Check the status page for incidents before reporting an outage"},
	{"Maintenance windows", "Scheduled maintenance is announced at least 48 hours ahead"},
	{"SLA credits", "Open a claim with incident dates to request SLA credits"},
	{"Mobile app setup", "Install the mobile app and scan the pairing code"},
	{"Notifications", "Choose email or mobile notifications per event type"},
	{"Keyboard shortcuts", "Press question mark anywhere to see keyboard shortcuts"},
	{"Dark mode", "Toggle dark mode from the appearance settings"},
	{"Shared workspaces", "Share a workspace with an external collaborator by email invite"},
	{"Audit trail", "Every admin action is written to the audit trail"},
	{"Sso setup", "Configure SSO with your identity provider metadata file"},
	{"Ip allowlist", "Restrict logins to your corporate IP allowlist"},
	{"Sandbox mode", "Use the sandbox to test changes safely before applying them"},
	{"Bulk import", "Import records in bulk from a CSV file"},
	{"Custom fields", "Add custom fields to tickets to match your workflow"},
	{"Priority levels", "Set priority levels so urgent issues surface first"},
}

var queryAxes = map[int][]string{
	0: {"refund", "refunds", "refunded"},
	1: {"cancel", "cancellation", "canceled"},
	2: {"login", "password", "credentials"},
	3: {"billing", "invoice", "card", "charge"},
	4: {"api", "webhook"},
	5: {"seat", "team", "teammate"},
	6: {"plan", "tier", "upgrade", "downgrade"},
}

func embedText(text string) []any {
	lower := " " + strings.ToLower(text) + " "
	lit := make([]bool, 8)
	for axis, keywords := range queryAxes {
		for _, kw := range keywords {
			if strings.Contains(lower, " "+kw) {
				lit[axis] = true
				break
			}
		}
	}
	vec := make([]any, 8)
	for i := range vec {
		vec[i] = 0.2
		if lit[i] {
			vec[i] = 1.0
		}
	}
	return vec
}

func axisAlone(text string, axis int) bool {
	vec := embedText(text)
	for i, v := range vec {
		on, _ := v.(float64)
		if i == axis && on != 1.0 {
			return false
		}
		if i != axis && on == 1.0 {
			return false
		}
	}
	return true
}

func articleRecord(a kbArticle) map[string]any {
	return map[string]any{
		"title":     a.title,
		"body":      a.body,
		"embedding": embedText(a.title + " " + a.body),
	}
}

func TestStage03IngestTheKB(t *testing.T) {
	server := mcpTool(t, "describe_server", map[string]any{})
	embedding, _ := server["embedding"].(map[string]any)
	if embedding == nil {
		t.Fatalf("describe_server: no embedding object: %v", server)
	}
	if provider := asStr(t, embedding["provider"], "describe_server.provider"); provider != "local" {
		t.Fatalf("describe_server provider: %q, the scenario runs the built-in local provider", provider)
	}
	if usable, _ := embedding["usable"].(bool); !usable {
		t.Fatalf("describe_server usable: %v", embedding["usable"])
	}
	model := asStr(t, embedding["model"], "describe_server.model")
	if identity := asStr(t, embedding["identity"], "describe_server.identity"); identity != "local/"+model {
		t.Fatalf("describe_server identity %q is not local/<model> for model %q", identity, model)
	}

	if len(kbCorpus) != 40 {
		t.Fatalf("kb corpus must hold 40 articles, has %d", len(kbCorpus))
	}
	batch := 10
	var ids []int64
	var lastCall map[string]any
	for start := 0; start < len(kbCorpus); start += batch {
		end := start + batch
		records := []any{}
		for _, a := range kbCorpus[start:end] {
			records = append(records, articleRecord(a))
		}
		call := map[string]any{
			"namespace": scenarioNamespace,
			"table":     "kb_articles",
			"records":   records,
		}
		if start+batch >= len(kbCorpus) {
			call["idempotency_key"] = "kb-ingest-final-batch"
			lastCall = call
		}
		data := mcpTool(t, "insert", call)
		got, _ := data["ids"].([]any)
		if len(got) != end-start {
			t.Fatalf("kb insert batch: wanted %d ids, got %v", end-start, data)
		}
		for _, id := range got {
			ids = append(ids, asInt(t, id, "kb insert id"))
		}
	}
	app.kbIDs = ids

	cancelIDs := []int64{}
	for i, a := range kbCorpus {
		if axisAlone(a.title+" "+a.body, 1) {
			cancelIDs = append(cancelIDs, ids[i])
		}
	}
	app.kbCancelID = cancelIDs
	if len(cancelIDs) == 0 {
		t.Fatal("no kb article lands on the cancel axis alone; the semantic search assertion needs one")
	}

	first := mcpTool(t, "insert", lastCall)
	retry := mcpTool(t, "insert", lastCall)
	firstIDs, _ := first["ids"].([]any)
	retryIDs, _ := retry["ids"].([]any)
	if len(firstIDs) != len(retryIDs) {
		t.Fatalf("idempotent retry returned a different id list: %v then %v", firstIDs, retryIDs)
	}
	for i := range firstIDs {
		if asInt(t, firstIDs[i], "first ids") != asInt(t, retryIDs[i], "retry ids") {
			t.Fatalf("idempotent retry returned a different id at %d: %v then %v", i, firstIDs, retryIDs)
		}
	}
	if inserted := asInt(t, retry["inserted"], "retry inserted"); inserted != 0 {
		t.Fatalf("idempotent retry inserted new rows: %v", retry)
	}
	if replayed, _ := retry["replayed"].(bool); !replayed {
		t.Fatalf("idempotent retry does not report replayed: %v", retry)
	}

	data := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT COUNT(*) AS n FROM kb_articles",
	})
	rows := asMapList(t, data["rows"], "kb count rows")
	if n := asInt(t, rows[0]["n"], "kb article count"); n != 40 {
		t.Fatalf("kb_articles holds %d rows, expected exactly the 40 ingested articles", n)
	}
}
