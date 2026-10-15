package files

import (
	"strings"
	"unicode/utf8"
)

func BaseName(filename string) string {
	if i := strings.LastIndexAny(filename, `/\`); i >= 0 {
		filename = filename[i+1:]
	}
	filename = strings.ToValidUTF8(filename, "_")
	var b strings.Builder
	for _, r := range filename {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > 255 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
