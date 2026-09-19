package value

import (
	"strings"
	"unicode/utf8"
)

func RawSize(v any) int {
	switch t := v.(type) {
	case []byte:
		return len(t)
	case string:
		return len(t)
	default:
		return 16
	}
}

func ApproxSize(v any) int {
	switch t := v.(type) {
	case string:
		return EncodedSize(t)
	case []byte:
		return len(t)
	default:
		return 16
	}
}

func EncodedSize(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] < 0x20:
			n += 6
		case s[i] == '"' || s[i] == '\\':
			n += 3
		}
	}
	if strings.Contains(s, " ") || strings.Contains(s, " ") {
		n += 4 * (strings.Count(s, " ") + strings.Count(s, " "))
	}
	if !utf8.ValidString(s) {
		for _, r := range s {
			if r == utf8.RuneError {
				n += 6
			}
		}
	}
	return n
}
