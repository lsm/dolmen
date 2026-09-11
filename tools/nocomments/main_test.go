package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func commentCount(t *testing.T, src string) int {
	t.Helper()
	s, err := scanSource([]byte(src))
	if err != nil {
		t.Fatalf("scanSource(%q): %v", src, err)
	}
	return len(s.comments)
}

func TestLineCommentsCount(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"package p\n\nvar x = 1\n", 0},
		{"package p\n// line\nvar x = 1 // trailing\n", 2},
		{"// Foo does things.\nfunc Foo() {}\n", 1},
		{"/* one */ var x = 1 /* two */\n", 2},
		{"/* // inert */ var x = 1\n", 1},
		{"// /* inert\nvar x = 1\n", 1},
		{"package p\n\nvar s = \"// not a comment /* nor this */\"\n", 0},
		{"package p\n\nvar s = `// not a comment\n/* nor this */`\n", 0},
		{"package p\n\nvar c = '/'\nvar d = '//'\nvar u = 'ሴ'\n", 0},
		{"package p\n\nvar e = '\\n'\nvar q = '\\''\nvar b = '\\\\'\n", 0},
		{"var x = 1 /* a /* b */ + 2\n", 1},
		{"//go:generate: explanation\n", 1},
		{"//nolint-anything\n", 1},
		{"//nolint:\n", 1},
		{"//nolint:foo!\n", 1},
		{"//go:generate mockgen -source a.go\n", 0},
		{"//go:build linux && !darwin\n", 0},
		{"//go:embed model/*.gguf\n", 0},
		{"//go:build\n", 0},
		{"var x = 1 //nolint:gocyclo,goconst // rationale\n", 0},
		{"//go:build linux\r\n\r\npackage p\r\n", 0},
		{"var x = 1 //nolint:gocyclo\r\n", 0},
		{"func f() {\n\t//go:generate echo ignored\n}\n", 1},
		{"var x = 1 //go:generate echo\n", 1},
		{"func f() {\n\t//nolint:gocyclo\n}\n", 0},
		{"// plain crlf comment\r\npackage p\r\n", 1},
		{"var x = 1 /* a /* b */ + 2 // real\n", 2},
		{"var x = 1 /* a /* b */ + 2\nvar s = \"// found\"\n", 1},
		{"//go:build ignore\n\npackage p\n", 0},
		{"package p\n\n//go:embed foo.txt\nvar embedded string\n", 0},
		{"package p\n\n//go:generate stringer -type=Kind\n", 0},
		{"package p\n\nvar x = 1 //nolint:gocyclo\n", 0},
		{"package p\n\nvar x = 1 //nolint\n", 0},
		{"package p\n\nvar x = 1 // go:build\n", 1},
		{"package p\n\nvar x = 1 //go:buildozer\n", 1},
		{"package p\n\nvar x = 1 //nolintx\n", 1},
		{"package p\n\n/*nolint*/\n", 1},
		{"x := \"abc\n// counted\n", 1},
		{"var s = `raw never closed // also not a comment\n", 0},
	} {
		if got := commentCount(t, tc.src); got != tc.want {
			t.Errorf("scanSource(%q) counted %d comments, want %d", tc.src, got, tc.want)
		}
	}
}

func TestScanErrors(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want string
	}{
		{"var x = 1 /* never closed\n", "unterminated block comment"},
		{"/* a /* b */ + 2 /* still open\n", "unterminated block comment"},
	} {
		if _, err := scanSource([]byte(tc.src)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("scanSource(%q) err = %v, want containing %q", tc.src, err, tc.want)
		}
	}
}

func TestScanErrorLine(t *testing.T) {
	_, err := scanSource([]byte("package p\n\nvar x = 1 /* open\n"))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want line 3", err)
	}
}

func TestStrip(t *testing.T) {
	for _, tc := range []struct {
		src, want string
	}{
		{"package p\n\n// gone\nvar x = 1\n", "package p\n\nvar x = 1\n"},
		{"package p\n\nvar x = 1 // gone\nvar y = 2\n", "package p\n\nvar x = 1\nvar y = 2\n"},
		{"package p\n\nvar x = 1 /* gone */ + 2\n", "package p\n\nvar x = 1 + 2\n"},
		{"package p\n\n/*\n * gone\n */\nvar x = 1\n", "package p\n\nvar x = 1\n"},
		{"package p\n\nvar a = 1\n// one\n// two\nvar b = 2\n", "package p\n\nvar a = 1\nvar b = 2\n"},
		{"package p\n\nvar a = 1\n\n// one\n\n// two\n\nvar b = 2\n", "package p\n\nvar a = 1\n\nvar b = 2\n"},
		{"//go:build ignore\n\n// gone\npackage p\n", "//go:build ignore\n\npackage p\n"},
		{"package p\n\nvar s = `keep // this\n\n\nand this`\n\n// gone\n", "package p\n\nvar s = `keep // this\n\n\nand this`\n"},
		{"package p\n\nvar s = \"keep // this\" // gone\n", "package p\n\nvar s = \"keep // this\"\n"},
		{"package p\n\nvar x = 1 // gone  \t\nvar y = 2\n", "package p\n\nvar x = 1\nvar y = 2\n"},
		{"package p\n\nvar/**/x = 1\n", "package p\n\nvar x = 1\n"},
		{"package p\n\nx := 1 /*\n*/ y := 2\n", "package p\n\nx := 1\ny := 2\n"},
		{"package p\n\nx := 1 /*\n*/\ny := 2\n", "package p\n\nx := 1\ny := 2\n"},
		{"package p\n\nx := 1 /* no newline */ y := 2\n", "package p\n\nx := 1 y := 2\n"},
		{"package p\n\nx := b</**/>= 2\n", "package p\n\nx := b< >= 2\n"},
		{"package p\n\nn := 1/**/.5\n", "package p\n\nn := 1 .5\n"},
		{"package p\n\nch := make/**/(chan int)\n", "package p\n\nch := make (chan int)\n"},
		{"package p\n\nvar s = \"a\"/**/ + \"b\"\n", "package p\n\nvar s = \"a\" + \"b\"\n"},
		{"// header\n// lines\n\npackage p\n", "package p\n"},
		{"package p\n\nvar s = `x`\n\n// gone\n", "package p\n\nvar s = `x`\n"},
	} {
		s, err := scanSource([]byte(tc.src))
		if err != nil {
			t.Fatalf("scanSource(%q): %v", tc.src, err)
		}
		if got := string(stripComments([]byte(tc.src), s)); got != tc.want {
			t.Errorf("strip(%q)\n= %q\nwant %q", tc.src, got, tc.want)
		}
	}
}

func TestStripIdempotent(t *testing.T) {
	src := "package p\n\n// one\nvar x = 1 /* two */\nvar s = `// raw\n/* raw */`\n// three\n"
	s, err := scanSource([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	once := stripComments([]byte(src), s)
	s2, err := scanSource(once)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if twice := stripComments(once, s2); string(twice) != string(once) {
		t.Errorf("strip not idempotent:\nonce  = %q\ntwice = %q", once, twice)
	}
}

func TestJudge(t *testing.T) {
	allow := map[string]int{"a.go": 5}
	for _, tc := range []struct {
		file  string
		count int
		ok    bool
	}{
		{"a.go", 0, true},
		{"b.go", 0, true},
		{"b.go", 1, false},
		{"a.go", 5, true},
		{"a.go", 4, true},
		{"a.go", 6, false},
	} {
		if msg := judge(tc.file, tc.count, allow); (msg == "") != tc.ok {
			t.Errorf("judge(%q, %d) = %q, want ok=%v", tc.file, tc.count, msg, tc.ok)
		}
	}
	if msg := judge("b.go", 2, allow); !strings.Contains(msg, "non-allowlisted") {
		t.Errorf("judge non-allowlisted msg = %q", msg)
	}
	if msg := judge("a.go", 6, allow); !strings.Contains(msg, "increased") {
		t.Errorf("judge increased msg = %q", msg)
	}
}

func TestReadAllowlistMissing(t *testing.T) {
	allow, err := readAllowlist(t.TempDir() + "/absent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(allow) != 0 {
		t.Errorf("want empty allowlist, got %v", allow)
	}
}

func TestReadAllowlistParse(t *testing.T) {
	path := t.TempDir() + "/allow.txt"
	content := "# seeded at landing\n\ninternal/store/store.go:12\nweird/dir:2 x.go:3\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	allow, err := readAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"internal/store/store.go": 12, "weird/dir:2 x.go": 3}
	if !reflect.DeepEqual(allow, want) {
		t.Errorf("allow = %v, want %v", allow, want)
	}
}

func TestReadAllowlistMalformed(t *testing.T) {
	path := t.TempDir() + "/allow.txt"
	if err := writeFile(path, "internal/store/store.go\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := readAllowlist(path); err == nil {
		t.Error("want error for entry without count")
	}
}
