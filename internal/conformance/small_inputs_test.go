package conformance

import (
	"strings"
	"testing"
)

func TestABatchWriteThatIsNotAnObjectSaysSo(t *testing.T) {
	h := newHarness(t)
	batchDocs(t, h)
	for _, writes := range []string{`[5]`, `[null]`, `["insert"]`} {
		code, msg := batchError(t, h, `{"namespace":"acme","writes":`+writes+`}`)
		if code != "invalid_request" || !strings.Contains(msg, "writes[0] must be an object") || strings.Contains(msg, "Go value") {
			t.Fatalf("writes %s: %s %q", writes, code, msg)
		}
	}
}
