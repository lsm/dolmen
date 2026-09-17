package conformance

import (
	"os"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func resolveEngine(getenv func(string) string) (string, error) {
	name := getenv("DOLMEN_ENGINE")
	if err := store.ValidateEngine(name); err != nil {
		return "", err
	}
	if name == "" {
		name = store.EngineSQLite
	}
	return name, nil
}

var activeEngine, activeEngineErr = resolveEngine(os.Getenv)

func testEngine(t *testing.T) string {
	t.Helper()
	if activeEngineErr != nil {
		t.Fatalf("DOLMEN_ENGINE: %v", activeEngineErr)
	}
	return activeEngine
}

func sqliteOnly(t *testing.T) {
	t.Helper()
	if name := testEngine(t); name != store.EngineSQLite {
		t.Skipf("engine %q: this fixture probes SQLite storage internals directly", name)
	}
}

func TestEngineKnobResolution(t *testing.T) {
	cases := []struct {
		env     map[string]string
		want    string
		wantErr string
	}{
		{env: nil, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": ""}, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": "sqlite"}, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": "postgres"}, wantErr: `unknown engine "postgres" (the available engine is "sqlite")`},
		{env: map[string]string{"DOLMEN_ENGINE": "banana"}, wantErr: `unknown engine "banana"`},
	}
	for _, c := range cases {
		lookup := c.env
		got, err := resolveEngine(func(string) string {
			if lookup == nil {
				return ""
			}
			return lookup["DOLMEN_ENGINE"]
		})
		if c.wantErr != "" {
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("DOLMEN_ENGINE=%q: error %v, want %q", c.env["DOLMEN_ENGINE"], err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("DOLMEN_ENGINE=%q: %v", c.env["DOLMEN_ENGINE"], err)
		}
		if got != c.want {
			t.Fatalf("DOLMEN_ENGINE=%q: engine %q, want %q", c.env["DOLMEN_ENGINE"], got, c.want)
		}
	}
}
