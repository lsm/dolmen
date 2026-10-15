package files

import (
	"strings"
	"testing"
)

func TestMediaTypeInference(t *testing.T) {
	for _, tc := range []struct {
		declared, filename string
		head               []byte
		want               string
	}{
		{"audio/ogg; codecs=opus", "x.bin", nil, "audio/ogg"},
		{"application/octet-stream", "Report.PDF", nil, "application/pdf"},
		{"", "notes.md", nil, "text/markdown"},
		{"", "blob", []byte("%PDF-1.7"), "application/pdf"},
		{"", "blob", []byte{0, 1, 2, 3}, "application/octet-stream"},
		{"not a type", "a.png", nil, "image/png"},
		{"*/*", "a.png", nil, "image/png"},
		{"audio/*", "x.ogg", nil, "audio/ogg"},
	} {
		if got := InferMediaType(tc.declared, tc.filename, tc.head); got != tc.want {
			t.Errorf("InferMediaType(%q, %q) = %q, want %q", tc.declared, tc.filename, got, tc.want)
		}
	}
}

func TestAPartFileNameIsReducedToAValidBaseName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\Users\me\report.pdf`, "report.pdf"},
		{"dir/sub/a.txt", "a.txt"},
		{"a\tb\x01c.txt", "abc.txt"},
		{"bad\xffbyte.txt", "bad_byte.txt"},
		{strings.Repeat("é", 200), strings.Repeat("é", 127)},
		{"dir/", ""},
	} {
		if got := BaseName(tc.in); got != tc.want {
			t.Errorf("BaseName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
