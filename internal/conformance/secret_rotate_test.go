package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	dolmen "github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/secret"
)

func rotationKey() []byte { return bytes.Repeat([]byte{9}, secret.KeySize) }

func keyIDOf(t *testing.T, key []byte) string {
	t.Helper()
	k, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return k.ID()
}

func (h *harness) useKeys(active []byte, retired ...[]byte) {
	h.t.Helper()
	k, err := secret.New(active, retired...)
	if err != nil {
		h.t.Fatal(err)
	}
	h.secretKey, h.secretKeySet = k, true
	h.reopen()
}

func seedRotation(t *testing.T, h *harness, ns string, n int) {
	t.Helper()
	h.seedTable(ns, "creds", secretTableFields())
	records := make([]map[string]any, 0, n+1)
	for i := 0; i < n; i++ {
		records = append(records, map[string]any{"label": fmt.Sprintf("row%d", i), "token": fmt.Sprintf("%s-%d", conformanceSecret, i), "emb": []float64{1, 0, 0}})
	}
	records = append(records, map[string]any{"label": "nulled", "emb": []float64{0, 1, 0}})
	h.mustHTTP("insert", map[string]any{"namespace": ns, "table": "creds", "records": records})
}

func storedKeyIDs(t *testing.T, h *harness, ns string) map[string]int {
	t.Helper()
	data := h.mustHTTP("query", map[string]any{"namespace": ns, "sql": "SELECT token AS raw FROM creds WHERE token IS NOT NULL"})
	out := map[string]int{}
	for _, r := range data["rows"].([]any) {
		raw, err := base64.StdEncoding.DecodeString(r.(map[string]any)["raw"].(string))
		if err != nil || len(raw) < 9 {
			t.Fatalf("stored secret is not a ciphertext blob: %v %v", r, err)
		}
		out[hex.EncodeToString(raw[1:9])]++
	}
	return out
}

func revealedTokens(t *testing.T, h *harness, ns string) map[string]any {
	t.Helper()
	data := h.mustHTTP("read_rows", map[string]any{"namespace": ns, "table": "creds", "ids": idsUpTo(100), "reveal": []string{"token"}})
	return tokensOf(t, data["rows"])
}

func idsUpTo(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func rotateUntilDone(t *testing.T, h *harness, body map[string]any) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		var data map[string]any
		if i%2 == 0 {
			data = h.mustHTTP("rotate_secret_key", body)
		} else {
			data = h.mustMCP("rotate_secret_key", body)
		}
		if data["done"] == true {
			return data
		}
	}
	t.Fatal("rotate_secret_key never reported done")
	return nil
}

func keyCounts(data map[string]any) map[string]float64 {
	out := map[string]float64{}
	for _, k := range data["keys"].([]any) {
		m := k.(map[string]any)
		out[m["key_id"].(string)] = m["values"].(float64)
	}
	return out
}

func TestRotateSecretKeyReencryptsResumablyAndKeepsPlaintext(t *testing.T) {
	h := newHarness(t)
	seedRotation(t, h, "sec", 5)
	seedRotation(t, h, "other", 3)
	idem := map[string]any{"namespace": "sec", "table": "creds", "idempotency_key": "rotate-idem", "records": []map[string]any{{"label": "idem", "token": conformanceSecret + "-idem"}}}
	first := h.mustHTTP("insert", idem)
	before := revealedTokens(t, h, "sec")
	oldID, newID := keyIDOf(t, conformanceKey()), keyIDOf(t, rotationKey())

	h.useKeys(rotationKey(), conformanceKey())
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "fresh", "token": conformanceSecret + "-fresh"}}})
	if got := storedKeyIDs(t, h, "sec"); got[oldID] != 6 || got[newID] != 1 {
		t.Fatalf("after the key change, new writes must use the new key and old values stay put: %v", got)
	}

	step := h.mustHTTP("rotate_secret_key", map[string]any{"namespace": "sec", "limit": 2})
	if step["rotated"].(float64) != 2 || step["remaining"].(float64) != 4 || step["done"] != false || step["active_key"] != newID {
		t.Fatalf("a bounded call must rotate its limit and report what remains: %v", step)
	}
	if c := keyCounts(step); c[oldID] != 4 || c[newID] != 3 {
		t.Fatalf("keys must count values per key id: %v", step["keys"])
	}
	noPlaintext(t, "rotate_secret_key", step)
	if hits := storageHoldsPlaintext(t, h); len(hits) > 0 {
		t.Fatalf("plaintext in storage mid-rotation: %v", hits)
	}

	h.reopen()
	done := rotateUntilDone(t, h, map[string]any{"namespace": "sec", "limit": 1})
	if c := keyCounts(done); c[oldID] != 0 || c[newID] != 7 {
		t.Fatalf("a resumed rotation must finish the namespace: %v", done)
	}
	if got := storedKeyIDs(t, h, "other"); got[oldID] != 3 {
		t.Fatalf("a namespace-scoped rotation must leave other namespaces alone: %v", got)
	}
	all := rotateUntilDone(t, h, map[string]any{})
	if c := keyCounts(all); c[oldID] != 0 || c[newID] != 10 || len(all["tables"].([]any)) != 2 {
		t.Fatalf("a server-wide rotation must cover every namespace: %v", all)
	}
	for _, ns := range []string{"sec", "other"} {
		if got := storedKeyIDs(t, h, ns); got[oldID] != 0 {
			t.Fatalf("%s still holds values under the old key: %v", ns, got)
		}
	}
	after := revealedTokens(t, h, "sec")
	for label, v := range before {
		if after[label] != v {
			t.Fatalf("reveal of %s changed across rotation: %v -> %v", label, v, after[label])
		}
	}
	if hits := storageHoldsPlaintext(t, h); len(hits) > 0 {
		t.Fatalf("plaintext in storage after rotation: %v", hits)
	}

	replay := h.mustHTTP("insert", idem)
	if replay["replayed"] != true {
		t.Fatalf("an idempotent insert made before the rotation must replay after it: %v", replay)
	}
	assertJSONEqual(t, "replayed ids", replay["ids"], first["ids"])

	h.useKeys(rotationKey())
	if got := revealedTokens(t, h, "sec"); got["row0"] != conformanceSecret+"-0" || got["fresh"] != conformanceSecret+"-fresh" {
		t.Fatalf("after dropping the old key, reveal must still work: %v", got)
	}
	final := h.mustHTTP("rotate_secret_key", map[string]any{})
	if c := keyCounts(final); final["done"] != true || len(c) != 1 || c[newID] != 10 {
		t.Fatalf("with the old key dropped, only the active key is referenced: %v", final)
	}
}

func TestRotateSecretKeyDroppedTooEarlyTeaches(t *testing.T) {
	h := newHarness(t)
	seedRotation(t, h, "sec", 3)
	oldID := keyIDOf(t, conformanceKey())
	h.useKeys(rotationKey(), conformanceKey())
	h.mustHTTP("rotate_secret_key", map[string]any{"limit": 1})

	h.useKeys(rotationKey())
	status, body := h.httpCall("rotate_secret_key", map[string]any{})
	env := envelopeOf(t, body)
	if status != http.StatusBadRequest || env["code"] != "conflict" {
		t.Fatalf("rotating with a referenced key missing: status %d, want a 400 conflict: %v", status, body)
	}
	msg := env["message"].(string)
	for _, want := range []string{oldID, secret.EnvOldKeys, "sec.creds"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not name %q", msg, want)
		}
	}
	res := h.mcpCall("rotate_secret_key", map[string]any{})
	if env := res.toolError(); env == nil || env["code"] != "conflict" {
		t.Fatalf("over MCP: %+v", res)
	}
	status, body = h.httpCall("read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": idsUpTo(3), "reveal": []string{"token"}})
	if status != http.StatusInternalServerError || envelopeOf(t, body)["code"] != "internal_error" {
		t.Fatalf("revealing a value under a dropped key stays internal_error: %d %v", status, body)
	}
	noPlaintext(t, "reveal under a dropped key", body)

	h.useKeys(rotationKey(), conformanceKey())
	if done := rotateUntilDone(t, h, map[string]any{}); done["remaining"].(float64) != 0 {
		t.Fatalf("adding the key back must let the rotation finish: %v", done)
	}
}

func TestRotateSecretKeyWithConcurrentWrites(t *testing.T) {
	h := newHarness(t)
	seedRotation(t, h, "sec", 40)
	h.useKeys(rotationKey(), conformanceKey())
	oldID := keyIDOf(t, conformanceKey())

	want := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				label := fmt.Sprintf("row%d", w*20+i)
				val := fmt.Sprintf("%s-w%d-%d", conformanceSecret, w, i)
				if _, err := h.httpData("update", map[string]any{"namespace": "sec", "table": "creds", "filter": "label = ?", "args": []any{label}, "set": map[string]any{"token": val}}); err != nil {
					errs <- err
					return
				}
				ins := fmt.Sprintf("new-w%d-%d", w, i)
				if _, err := h.httpData("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": ins, "token": val + "-ins"}}}); err != nil {
					errs <- err
					return
				}
				mu.Lock()
				want[label], want[ins] = val, val+"-ins"
				mu.Unlock()
			}
		}(w)
	}
	for i := 0; i < 30; i++ {
		if _, err := h.httpData("rotate_secret_key", map[string]any{"namespace": "sec", "limit": 3}); err != nil {
			t.Fatalf("rotation during writes: %v", err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("write during rotation: %v", err)
	}
	rotateUntilDone(t, h, map[string]any{"namespace": "sec"})
	if got := storedKeyIDs(t, h, "sec"); got[oldID] != 0 {
		t.Fatalf("values left under the old key: %v", got)
	}
	got := revealedTokens(t, h, "sec")
	for label, v := range want {
		if got[label] != v {
			t.Fatalf("%s reveals %v, want %v", label, got[label], v)
		}
	}
	for i := 20 - 5; i < 20; i++ {
		label := fmt.Sprintf("row%d", i)
		if got[label] != fmt.Sprintf("%s-%d", conformanceSecret, i) {
			t.Fatalf("untouched %s reveals %v", label, got[label])
		}
	}
	if hits := storageHoldsPlaintext(t, h); len(hits) > 0 {
		t.Fatalf("plaintext in storage: %v", hits)
	}
}

func TestRotateSecretKeyRefusals(t *testing.T) {
	h := newHarness(t)
	h.secretKey, h.secretKeySet = nil, true
	h.reopen()
	status, body := h.httpCall("rotate_secret_key", map[string]any{})
	if status != http.StatusBadRequest || !strings.Contains(envelopeOf(t, body)["message"].(string), secret.EnvKey) {
		t.Fatalf("without a key: %d %v", status, body)
	}
	h.useKeys(rotationKey())
	status, body = h.httpCall("rotate_secret_key", map[string]any{"namespace": "missing"})
	if status != http.StatusNotFound {
		t.Fatalf("unknown namespace: %d %v", status, body)
	}
	status, _ = h.httpCall("rotate_secret_key", map[string]any{"limit": 0})
	if status != http.StatusBadRequest {
		t.Fatalf("limit 0: %d", status)
	}
	empty := h.mustHTTP("rotate_secret_key", map[string]any{})
	if empty["done"] != true || empty["rotated"].(float64) != 0 {
		t.Fatalf("nothing to rotate: %v", empty)
	}
}

func TestRotateSecretKeyEmbedded(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	emb := openEmbedded(t, dir, dolmen.WithSecretKey(conformanceKey()))
	if _, err := emb.CreateTable(ctx, "sec", "creds", []dolmen.Field{{Name: "label", Type: "string"}, {Name: "token", Type: "secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := emb.Insert(ctx, "sec", "creds", []map[string]any{{"label": "alpha", "token": conformanceSecret}, {"label": "beta", "token": conformanceSecret + "-b"}}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	emb.Close()

	emb = openEmbedded(t, dir, dolmen.WithSecretKey(rotationKey(), conformanceKey()))
	step, err := emb.RotateSecretKey(ctx, "sec", 1)
	if err != nil || step.Rotated != 1 || step.Remaining != 1 || step.Keys[keyIDOf(t, conformanceKey())] != 1 {
		t.Fatalf("bounded facade rotation: %+v %v", step, err)
	}
	done, err := emb.RotateSecretKey(ctx, "sec", 0)
	if err != nil || done.Remaining != 0 || done.Keys[keyIDOf(t, rotationKey())] != 2 || len(done.Tables) != 1 {
		t.Fatalf("facade rotation: %+v %v", done, err)
	}
	emb.Close()

	emb = openEmbedded(t, dir, dolmen.WithSecretKey(rotationKey()))
	defer emb.Close()
	plain, err := emb.RevealRows(ctx, "sec", "creds", []int64{1, 2}, []string{"token"})
	if err != nil || plain.Rows[0]["token"] != conformanceSecret || plain.Rows[1]["token"] != conformanceSecret+"-b" {
		t.Fatalf("facade reveal after rotation without the old key: %v %v", plain.Rows, err)
	}
	if _, err := emb.RotateSecretKey(ctx, "sec", -1); err == nil {
		t.Fatal("a negative limit must be refused")
	}
}
