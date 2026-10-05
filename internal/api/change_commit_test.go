package api

import (
	"encoding/json"
	"github.com/lsm/dolmen/internal/store"
	"testing"
)

func TestChangeCommitIsOptionalInAllProjections(t *testing.T) {
	records := []store.ChangeRecord{{Table: "notes", RowID: 1, Kind: store.ChangeInsert}, {Table: "notes", RowID: 2, Kind: store.ChangeInsert, Commit: 7}}
	for i, raw := range renderChanges(records, "next")["changes"].([]map[string]any) {
		commit, ok := raw["commit"]
		if i == 0 && ok || i == 1 && (!ok || commit != int64(7)) {
			t.Fatalf("projection %d: %v", i, raw)
		}
		encoded, err := sseJSON(sseChange{Table: records[i].Table, RowID: records[i].RowID, Kind: string(records[i].Kind), Commit: records[i].Commit})
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := json.Unmarshal(encoded, &frame); err != nil {
			t.Fatal(err)
		}
		_, ok = frame["commit"]
		if ok != (i == 1) {
			t.Fatalf("SSE projection %d: %v", i, frame)
		}
	}
	for _, out := range []map[string]any{changesOutSchema, changesPageOutSchema()} {
		props := out["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["commit"]; !ok {
			t.Fatalf("discovery schema lacks commit: %v", out)
		}
	}
}
