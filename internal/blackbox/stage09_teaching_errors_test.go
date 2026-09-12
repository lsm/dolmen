package blackbox

import (
	"testing"
)

const crossFeedTeaching = `cursor was minted on a different feed (a specific table's, or the namespace-wide feed); pass it only to the feed you received it from — honoring it elsewhere would silently skip events — or start fresh with no cursor / "begin"`

const unknownCursorTeaching = `cursor is unknown or past the change-log retention window (-change-retention, default 168h); catch up by calling changes_since with no cursor to resume from the current head, or with cursor "begin" to replay retained history`

func TestStage09TeachingErrors(t *testing.T) {
	second, err := bootServer(t.TempDir(), mainRetention, false)
	if err != nil {
		t.Fatalf("boot the second short-retention instance: %v", err)
	}
	main := app.srv
	app.srv = second
	app.openapi = nil
	app.schemas = nil
	defer func() {
		if err := second.stop(); err != nil {
			t.Errorf("stopping the second instance: %v", err)
		}
		app.srv = main
		app.openapi = nil
		app.schemas = nil
	}()

	op(t, "create_table", map[string]any{
		"namespace": "nightshift/probe",
		"table":     "feed_mint",
		"fields": []any{
			map[string]any{"name": "note", "type": "string"},
		},
	})
	op(t, "insert", map[string]any{
		"namespace": "nightshift/probe",
		"table":     "feed_mint",
		"records":   []any{map[string]any{"note": "mint a cursor on each feed"}},
	})

	nsHead := op(t, "changes_since", map[string]any{"namespace": "nightshift/probe"})
	nsCursor := asStr(t, nsHead["next_cursor"], "namespace head cursor")
	tablePage := op(t, "changes_since", map[string]any{"namespace": "nightshift/probe", "table": "feed_mint"})
	tableCursor := asStr(t, tablePage["next_cursor"], "table head cursor")

	code, envelope := opErrorEnvelope(t, "changes_since", map[string]any{
		"namespace": "nightshift/probe",
		"cursor":    tableCursor,
	})
	if code != 400 {
		t.Fatalf("cross-feed reuse: HTTP %d", code)
	}
	assertTeachingError(t, envelope, "cross-feed", crossFeedTeaching)

	code, envelope = opErrorEnvelope(t, "changes_since", map[string]any{
		"namespace": "nightshift/probe",
		"table":     "feed_mint",
		"cursor":    nsCursor,
	})
	if code != 400 {
		t.Fatalf("cross-feed reuse in reverse: HTTP %d", code)
	}
	assertTeachingError(t, envelope, "cross-feed reverse", crossFeedTeaching)

	code, envelope = opErrorEnvelope(t, "changes_since", map[string]any{
		"namespace": "nightshift/probe",
		"cursor":    "not-a-cursor-the-server-ever-minted",
	})
	if code != 400 {
		t.Fatalf("unknown cursor: HTTP %d", code)
	}
	assertTeachingError(t, envelope, "unknown cursor", unknownCursorTeaching)

	reqHeaders := map[string]string{"X-Request-Id": "nightshift-teaching-errors-0001"}
	_, header, raw := postRawBody(t, app.srv.url+"/v1/insert", reqHeaders, `{"namespace": "nightshift/probe", "table":`)
	var envelopeRaw map[string]any
	decodeInto(t, raw, &envelopeRaw, "malformed json envelope")
	assertConforms(t, openapiSchema(t, "ErrorEnvelope"), envelopeRaw, "malformed json error envelope")
	errObj, _ := envelopeRaw["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("malformed json response has no error object: %s", raw)
	}
	if requestID := asStr(t, errObj["request_id"], "request_id"); requestID != "nightshift-teaching-errors-0001" {
		t.Fatalf("error request_id %q does not echo the X-Request-Id header", requestID)
	}
	if header.Get("X-Request-Id") != "nightshift-teaching-errors-0001" {
		t.Fatalf("response X-Request-Id %q does not echo the request header", header.Get("X-Request-Id"))
	}
}

func assertTeachingError(t *testing.T, envelope map[string]any, what string, wantMessage string) {
	t.Helper()
	if ok, _ := envelope["ok"].(bool); ok {
		t.Fatalf("%s: expected an error envelope, got %v", what, envelope)
	}
	errObj, _ := envelope["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("%s: no error object: %v", what, envelope)
	}
	if code := asStr(t, errObj["code"], what+" code"); code != "invalid_request" {
		t.Fatalf("%s: error code %q", what, code)
	}
	if msg := asStr(t, errObj["message"], what+" message"); msg != wantMessage {
		t.Fatalf("%s: teaching message mismatch:\n got: %q\nwant: %q", what, msg, wantMessage)
	}
	if rid := asStr(t, errObj["request_id"], what+" request_id"); rid == "" {
		t.Fatalf("%s: error carries no request_id", what)
	}
	assertConforms(t, openapiSchema(t, "ErrorEnvelope"), envelope, what+" envelope")
}
