package schema

import (
	"fmt"
	"strings"
	"unicode"
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
	if rule := FileNameRule(name); rule != "" {
		return fmt.Errorf("field %q: %s", field, rule)
	}
	return nil
}

func FileNameRule(name string) string {
	switch {
	case name == "" || len(name) > 255:
		return "a file name must be 1 to 255 bytes"
	case !utf8.ValidString(name):
		return "a file name must be valid UTF-8"
	case name == "." || name == "..":
		return "a file name cannot be . or ..; give the base name of a file, such as report.pdf"
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '/' || r == '\\' {
			return "a file name cannot contain control characters, bidirectional controls, / or \\; give the base name only, such as report.pdf"
		}
	}
	return ""
}
