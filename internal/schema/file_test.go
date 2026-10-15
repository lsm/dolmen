package schema

import (
	"strings"
	"testing"
)

func TestFileFieldDeclarationRules(t *testing.T) {
	ok := []Field{
		{Name: "doc", Type: File},
		{Name: "doc", Type: File, MaxBytes: 1024, MediaTypes: []string{"application/pdf", "audio/*"}},
		{Name: "doc", Type: File, Required: true},
	}
	for _, f := range ok {
		if err := Validate([]Field{f}); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	bad := []struct {
		f    Field
		want string
	}{
		{Field{Name: "doc", Type: File, Fulltext: true}, "fulltext is not allowed on file fields"},
		{Field{Name: "doc", Type: File, Vectorize: true}, "vectorize is not allowed on file fields"},
		{Field{Name: "doc", Type: File, Enum: []string{"a"}}, "enum is not allowed on file fields"},
		{Field{Name: "doc", Type: File, Shape: "object"}, "shape is not allowed on file fields"},
		{Field{Name: "doc", Type: File, Default: "x"}, "default is not allowed on file fields"},
		{Field{Name: "doc", Type: File, MaxBytes: -1}, "max_bytes must be positive"},
		{Field{Name: "doc", Type: File, MediaTypes: []string{}}, "at least one type"},
		{Field{Name: "doc", Type: File, MediaTypes: []string{"PDF"}}, "is not a media type"},
		{Field{Name: "doc", Type: File, MediaTypes: []string{"*/*"}}, "is not a media type"},
		{Field{Name: "doc", Type: File, MediaTypes: []string{"audio/*", "audio/*"}}, "twice"},
		{Field{Name: "doc", Type: String, MaxBytes: 5}, "only allowed on file fields"},
		{Field{Name: "doc", Type: JSON, MediaTypes: []string{"audio/*"}}, "only allowed on file fields"},
	}
	for _, tc := range bad {
		err := Validate([]Field{tc.f})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, want %q", tc.f, err, tc.want)
		}
	}
}

func TestMediaTypeAllowed(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		mt       string
		want     bool
	}{
		{nil, "anything/at-all", true},
		{[]string{"audio/*"}, "audio/ogg", true},
		{[]string{"audio/*"}, "audiox/ogg", false},
		{[]string{"application/pdf"}, "application/pdf", true},
		{[]string{"application/pdf"}, "application/pdfx", false},
	} {
		if got := MediaTypeAllowed(tc.patterns, tc.mt); got != tc.want {
			t.Errorf("MediaTypeAllowed(%v, %q) = %v", tc.patterns, tc.mt, got)
		}
	}
}
