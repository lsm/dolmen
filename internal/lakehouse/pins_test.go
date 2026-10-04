package lakehouse

import (
	"encoding/json"
	"os/exec"
	"testing"
)

func TestDependencyPins(t *testing.T) {
	for module, want := range map[string]string{
		"github.com/apache/iceberg-go":     "v0.6.0",
		"github.com/apache/arrow-go/v18":   "v18.6.0",
		"github.com/parquet-go/parquet-go": "v0.32.0",
	} {
		t.Run(module, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-json", module).CombinedOutput()
			if err != nil {
				t.Fatalf("required lakehouse pin %s=%s missing: %v: %s", module, want, err, out)
			}
			var resolved struct {
				Version string
				Replace *struct{ Version string }
			}
			if err := json.Unmarshal(out, &resolved); err != nil {
				t.Fatal(err)
			}
			version := resolved.Version
			if resolved.Replace != nil {
				version = resolved.Replace.Version
			}
			if version != want {
				t.Fatalf("lakehouse pin %s=%s resolved to %q; iceberg-go v0.6.0 requires Arrow v18.6.0 because newer Arrow selects twmb/avro v1.8.0 with an incompatible SchemaNode.Root signature; upgrade the pins together", module, want, version)
			}
		})
	}
}
