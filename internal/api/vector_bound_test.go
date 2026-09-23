package api

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestAnOversizedQueryVectorIsRefusedBeforeItIsDecoded(t *testing.T) {
	for _, key := range []string{"vector", "Vector", "VECTOR"} {
		t.Run(key, func(t *testing.T) {
			refusesBeforeDecoding(t, []byte(`{"namespace":"ns","table":"t","`+key+`":[`+strings.Repeat("0.5,", 2_000_000)+`0.5]}`))
		})
	}
}

func refusesBeforeDecoding(t *testing.T, body []byte) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Ops["search_vector"].Func(context.Background(), New(nil, nil), body)
	runtime.ReadMemStats(&after)
	got := WrapError(err)
	if got.Code != ErrCodeInvalid || !strings.Contains(got.Message, "vector has more than 4096 numbers") {
		t.Fatalf("got %s %q", got.Code, got.Message)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("refusing a 2,000,001-number vector allocated %d bytes; the length check must run before the request is decoded", grew)
	}
}

func TestLongestVectorSeesEveryVectorKey(t *testing.T) {
	cases := map[string]int{
		`{"vector":[1,2,3]}`:                               3,
		`{"text":"x"}`:                                     0,
		`{"vector":[1],"vector":[1,2,3,4,5]}`:              5,
		`{"args":[[1,2,3,4,5,6]],"vector":[1,2]}`:          2,
		`{"vector":[[1,2],[3]]}`:                           2,
		`not json`:                                         0,
		`{"vector":[` + strings.Repeat("1,", 5000) + `1]}`: schema.MaxVectorDim + 1,
		`{"Vector":[1],"vECTOR":[` + strings.Repeat("1,", 5000) + `1]}`: schema.MaxVectorDim + 1,
	}
	for body, want := range cases {
		if got := longestVector([]byte(body), schema.MaxVectorDim); got != want {
			t.Errorf("%.60s: got %d, want %d", body, got, want)
		}
	}
}
