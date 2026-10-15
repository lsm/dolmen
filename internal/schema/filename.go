package schema

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func ValidateMediaType(field, m string) error {
	typ, sub, ok := strings.Cut(m, "/")
	if !ok || !mediaToken(typ) || !mediaToken(sub) {
		return fmt.Errorf("field %q: media_type %q is not a media type: use lowercase type/subtype, such as application/pdf", field, m)
	}
	return nil
}

func mediaToken(s string) bool {
	if s == "" || len(s) > 127 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && strings.ContainsRune("!#$&^_.+-", r):
		default:
			return false
		}
	}
	return true
}

func ValidateFileName(field, name string) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("field %q: a file name must be 1 to 255 bytes", field)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("field %q: a file name must be valid UTF-8", field)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return fmt.Errorf("field %q: a file name cannot contain control characters, / or \\; give the base name only, such as report.pdf", field)
		}
	}
	return nil
}
