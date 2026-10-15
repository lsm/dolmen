package schema

import (
	"strings"
	"testing"
)

func TestFileNameRules(t *testing.T) {
	for _, name := range []string{"report.pdf", "naïve plan.txt", strings.Repeat("a", 255)} {
		if err := ValidateFileName("doc", name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", strings.Repeat("a", 256), "dir/x", `dir\x`, "a\nb", "\xff"} {
		if err := ValidateFileName("doc", name); err == nil {
			t.Errorf("%q should be refused", name)
		}
	}
}

func TestMediaTypeRules(t *testing.T) {
	for _, m := range []string{"application/pdf", "audio/ogg", "application/vnd.ms-excel", "image/svg+xml"} {
		if err := ValidateMediaType("doc", m); err != nil {
			t.Errorf("%q: %v", m, err)
		}
	}
	for _, m := range []string{"pdf", "Application/PDF", "audio/*", "/ogg", "audio/", "a/b c"} {
		if err := ValidateMediaType("doc", m); err == nil {
			t.Errorf("%q should be refused", m)
		}
	}
}
