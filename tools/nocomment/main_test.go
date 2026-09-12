package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanIgnoresMarkersInsideLiterals(t *testing.T) {
	src := "package p\n\nvar a = \"// not a comment\"\nvar b = `/* neither */`\nvar c = '/'\nvar d = '*'\nvar e = \"/*\"\n"
	if got := scan([]byte(src)); len(got) != 0 {
		t.Fatalf("scan() = %d spans, want 0", len(got))
	}
}

func TestScanCountsLineAndBlockComments(t *testing.T) {
	src := "package p\n\n// doc\nfunc f() {} // trailing\n\n/* block\nspans */\n"
	if got := scan([]byte(src)); len(got) != 3 {
		t.Fatalf("scan() = %d spans, want 3", len(got))
	}
}

func TestScanExemptsDirectivesOnly(t *testing.T) {
	src := "//go:build linux\n// +build linux\n\npackage p\n\n//go:generate go vet ./...\n//go:embed x.txt\nvar x string\n\n// ordinary\n"
	got := scan([]byte(src))
	if len(got) != 1 {
		t.Fatalf("scan() = %d spans, want 1", len(got))
	}
	if lit := src[got[0].start:got[0].end]; lit != "// ordinary" {
		t.Fatalf("span = %q, want %q", lit, "// ordinary")
	}
}

func TestScanTreatsDirectiveLookalikesAsComments(t *testing.T) {
	src := "//go:buildings are tall\n//go:generateX\n// +buildinfo\n//go:embedding\n//go:build(x)\n//go:generateX \n\npackage p\n"
	if got := scan([]byte(src)); len(got) != 6 {
		t.Fatalf("scan() = %d spans, want 6", len(got))
	}
}

func TestScanExemptsIndentedAndSpaceOptionalConstraints(t *testing.T) {
	src := "\t//go:build linux\n\t// +build linux\n//+build linux\n//go:build\flinux\n//go:build\n\npackage p\n"
	if got := scan([]byte(src)); len(got) != 0 {
		t.Fatalf("scan() = %d spans, want 0", len(got))
	}
}

func TestScanRequiresBlankLineForPlusBuild(t *testing.T) {
	if got := scan([]byte("// +build linux\npackage p\n\nvar X = 1\n")); len(got) != 1 {
		t.Fatalf("scan() = %d spans without a blank line, want 1", len(got))
	}
	if got := scan([]byte("// +build linux\n\npackage p\n\nvar X = 1\n")); len(got) != 0 {
		t.Fatalf("scan() = %d spans with a blank line, want 0", len(got))
	}
}

func TestScanStopsPlusBuildAtBlockComment(t *testing.T) {
	src := "// +build linux\n/* x */\n\npackage p\n\nvar X = 1\n"
	if got := scan([]byte(src)); len(got) != 2 {
		t.Fatalf("scan() = %d spans, want 2", len(got))
	}
}

func TestScanRequiresExactGoBuildPrefix(t *testing.T) {
	src := "// go:build linux\n//\tgo:build linux\n\npackage p\n\nvar X = 1\n"
	if got := scan([]byte(src)); len(got) != 2 {
		t.Fatalf("scan() = %d spans, want 2", len(got))
	}
}

func TestScanDoesNotExemptConstraintAfterPackage(t *testing.T) {
	src := "//go:build linux\n\npackage p\n\nfunc f() {\n\t//go:build linux\n}\n"
	got := scan([]byte(src))
	if len(got) != 1 {
		t.Fatalf("scan() = %d spans, want 1", len(got))
	}
	if lit := src[got[0].start:got[0].end]; lit != "//go:build linux" {
		t.Fatalf("span = %q, want the post-package constraint", lit)
	}
}

func TestScanDoesNotExemptSameLineConstraint(t *testing.T) {
	src := []byte("/* note */ //go:build linux\n\npackage p\n\nvar X = 1\n")
	if got := scan(src); len(got) != 2 {
		t.Fatalf("scan() = %d spans, want 2", len(got))
	}
}

func TestScanRequiresDirectiveLinePosition(t *testing.T) {
	src := "package p\n\n//go:generate echo ok\nfunc f() {\n\t//go:generate echo indented\n\ttype S struct {\n\t\t//go:embed x.txt\n\t\tF string\n\t}\n\tvar a = 1\n\ta = 2 //go:generate echo trailing\n\ta = 3 //go:embed x.txt\n\t_ = a\n\t_ = S{}\n}\n"
	if got := scan([]byte(src)); len(got) != 3 {
		t.Fatalf("scan() = %d spans, want 3", len(got))
	}
}

func TestScanRequiresEmbedAndGenerateSeparators(t *testing.T) {
	src := "package p\n\n//go:generate\n//go:generate\tfalse\n//go:embed\n//go:embed\u00a0x.txt\n//go:generate\u00a0false\nvar A = 1\n"
	if got := scan([]byte(src)); len(got) != 3 {
		t.Fatalf("scan() = %d spans, want 3", len(got))
	}
}

func TestLoadAllowlistSkipsBlanksAndComments(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "allowlist.txt"), "\n# note\n\na/b.go  \n")
	got, err := loadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["a/b.go"] {
		t.Fatalf("loadAllowlist() = %v, want {a/b.go}", got)
	}
}

func TestCheckAllowlistRatchet(t *testing.T) {
	dir := t.TempDir()
	commented := writeFile(t, filepath.Join(dir, "commented.go"), "package p\n\n// x\nvar X = 1\n")
	clean := writeFile(t, filepath.Join(dir, "clean.go"), "package p\n\nvar Y = 1\n")
	allowPath := filepath.Join(dir, "allowlist.txt")

	cases := []struct {
		name  string
		allow string
		files []string
		want  int
	}{
		{"non-allowlisted comment fails", "", []string{commented, clean}, 1},
		{"allowlisted comment passes", commented + "\n", []string{commented, clean}, 0},
		{"allowlist addition for comment-free file fails", commented + "\n" + clean + "\n", []string{commented, clean}, 1},
		{"allowlist entry for a missing file fails", filepath.Join(dir, "gone.go") + "\n", []string{commented, clean}, 1},
		{"comment-free file with no entry passes", "", []string{clean}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, allowPath, tc.allow)
			if got := checkAllowlist(tc.files, allowPath); got != tc.want {
				t.Fatalf("checkAllowlist() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheckAllowlistMissingFile(t *testing.T) {
	dir := t.TempDir()
	clean := writeFile(t, filepath.Join(dir, "clean.go"), "package p\n\nvar Y = 1\n")
	if got := checkAllowlist([]string{clean}, filepath.Join(dir, "absent.txt")); got != 2 {
		t.Fatalf("checkAllowlist() = %d, want 2", got)
	}
}

func TestGoFilesListsTrackedSources(t *testing.T) {
	files, err := goFiles("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if path == "tools/nocomment/main.go" {
			return
		}
	}
	t.Fatalf("goFiles() = %d files, missing tools/nocomment/main.go", len(files))
}

func TestLoadBearingMatchesToolchainDirectiveGrammar(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"//go:linkname f runtime.f", true},
		{"//go:wasmimport env add", true},
		{"//go:noinline", true},
		{"//go:nosplit", true},
		{"//go:build", true},
		{"//go:embed x.txt", true},
		{"//go:generate", true},
		{"//line foo.go:1", true},
		{"//export Foo", true},
		{"//extern foo", true},
		{"//go:buildings are tall", true},
		{"// go:build linux", false},
		{"//go:", false},
		{"//go", false},
		{"//note: x", false},
		{"// TODO: x", false},
		{"//http://x", false},
		{"//x86:y", true},
		{"/*line generated.go:900*/", true},
		{"/*line :12*/", true},
		{"/*go:linkname x*/", false},
		{"/* ordinary */", false},
		{"/*lines tall*/", false},
	}
	for _, tc := range cases {
		if got := loadBearing([]byte(tc.text), span{start: 0, end: len(tc.text)}); got != tc.want {
			t.Fatalf("loadBearing(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestStripKeepsSemanticCompilerDirectives(t *testing.T) {
	src := "package p\n\n//go:linkname f runtime.f\nfunc f() {}\n\n// doc\nfunc g() {}\n\n//go:wasmimport env add\nfunc add(a, b int) int {\n\treturn a + b\n}\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, kept := range []string{"//go:linkname f runtime.f", "//go:wasmimport env add"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("strip() dropped %q:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "// doc") {
		t.Fatalf("strip() kept an ordinary comment:\n%s", got)
	}
}

func TestStripKeepsDirectiveFamilyLookalikes(t *testing.T) {
	out, err := strip([]byte("package p\n\n//go:buildings are tall\nvar X = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "//go:buildings are tall") {
		t.Fatalf("strip() = %q, want the directive-family lookalike kept", got)
	}
}

func TestCommentEndHandlesLeadingSlashBlockComment(t *testing.T) {
	src := []byte("package p\n\n/*/ c */\nvar X = 1\n")
	got := scan(src)
	if len(got) != 1 {
		t.Fatalf("scan() = %d spans, want 1", len(got))
	}
	if lit := string(src[got[0].start:got[0].end]); lit != "/*/ c */" {
		t.Fatalf("span = %q, want %q", lit, "/*/ c */")
	}
}

func TestStripHandlesLeadingSlashBlockComment(t *testing.T) {
	out, err := strip([]byte("package p\n\n/*/ c */\nvar X = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "/*") || !strings.Contains(got, "var X = 1") {
		t.Fatalf("strip() = %q, want the comment gone and the declaration kept", got)
	}
}

func TestStripKeepsCgoPreamble(t *testing.T) {
	src := []byte("package p\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\n\n// helper\nfunc F() {}\n")
	out, err := strip(src)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "#include <stdlib.h>") {
		t.Fatalf("strip() = %q, want the cgo preamble kept", got)
	}
	if strings.Contains(got, "// helper") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRemovesCommentAtEOFWithoutNewline(t *testing.T) {
	out, err := strip([]byte("package p\n\nvar X = 1\n// tail"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "package p\n\nvar X = 1\n" {
		t.Fatalf("strip() = %q", got)
	}
}

func TestStripSeparatesTokensAroundInlineComment(t *testing.T) {
	out, err := strip([]byte("package p\n\nvar/* c */x int\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "var x int") {
		t.Fatalf("strip() = %q, want the declaration separated", got)
	}
}

func TestStripPreservesStatementSeparator(t *testing.T) {
	out, err := strip([]byte("package p\n\nfunc f() {\n\tx := 1 /* c\n*/ _ = x\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "x := 1") || !strings.Contains(got, "_ = x") {
		t.Fatalf("strip() = %q, want both statements", got)
	}
}

func TestStripHandlesCRLFBlockComment(t *testing.T) {
	out, err := strip([]byte("package p\r\n\r\nvar X = 1 /* a\r\nb */\r\nvar Y = 2\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "*/") || !strings.Contains(got, "var X = 1") || !strings.Contains(got, "var Y = 2") {
		t.Fatalf("strip() = %q, want both declarations and no comment", got)
	}
}

func TestStripHandlesCRLFLineComment(t *testing.T) {
	out, err := strip([]byte("package p\r\n\r\n// gone\r\nvar X = 1\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "// gone") || !strings.Contains(got, "var X = 1") {
		t.Fatalf("strip() = %q, want the comment gone and the declaration kept", got)
	}
}

func TestStripKeepsLiteralCommentMarkers(t *testing.T) {
	src := "package p\n\nvar A = \"// keep\"\nvar B = `/* keep */`\n\n// gone\nvar C = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, kept := range []string{"// keep", "/* keep */"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("strip dropped %q:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip kept the comment:\n%s", got)
	}
}

func TestStripKeepsCodeAroundInlineBlockComment(t *testing.T) {
	out, err := strip([]byte("package p\n\nvar X = 1 /* mid */ + 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "/*") || !strings.Contains(got, "= 1") || !strings.Contains(got, "+ 2") {
		t.Fatalf("strip() = %q, want the expression preserved without the comment", got)
	}
}

func TestStripRemovesCommentsKeepsDirectivesAndLiterals(t *testing.T) {
	src := "//go:build linux\n\npackage p\n\n// doc\nfunc f() { // trailing\n\t/* inner */\n\tx := \"// literal\"\n\t_ = x\n}\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, kept := range []string{"//go:build linux", "// literal"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("strip dropped %q:\n%s", kept, got)
		}
	}
	for _, gone := range []string{"// doc", "// trailing", "/* inner */"} {
		if strings.Contains(got, gone) {
			t.Fatalf("strip kept %q:\n%s", gone, got)
		}
	}
}

func TestStripLeavesCommentFreeSourceUntouched(t *testing.T) {
	src := []byte("package p\n\nvar X   =   1\n")
	out, err := strip(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("strip() = %q, want the input byte-identical", out)
	}
}

func TestStripDoesNotActivateDormantConstraint(t *testing.T) {
	out, err := strip([]byte("// +build never\n/* x */\n\npackage p\n\nvar X = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "build never") {
		t.Fatalf("strip() = %q, want the inert constraint gone", got)
	}
}

func TestCheckEquivalentRejectsProgramChange(t *testing.T) {
	if err := checkEquivalent([]byte("package p\n\nvar X = 1\n"), []byte("package p\n\nvar X = 2\n")); err == nil {
		t.Fatal("checkEquivalent() accepted a changed program")
	}
}

func TestCheckEquivalentRejectsConstraintChange(t *testing.T) {
	before := []byte("//go:build linux\n\npackage p\n\nvar X = 1\n")
	after := []byte("//go:build darwin\n\npackage p\n\nvar X = 1\n")
	if err := checkEquivalent(before, after); err == nil {
		t.Fatal("checkEquivalent() accepted changed build constraints")
	}
}

func TestCheckEquivalentAcceptsCommentOnlyChange(t *testing.T) {
	if err := checkEquivalent([]byte("package p\n\n// c\nvar X = 1\n"), []byte("package p\n\nvar X = 1\n")); err != nil {
		t.Fatalf("checkEquivalent() = %v, want nil", err)
	}
}

func TestCheckEquivalentAcceptsBuildTagSync(t *testing.T) {
	before := []byte("// +build linux\n\npackage p\n\nvar X = 1\n")
	after := []byte("//go:build linux\n// +build linux\n\npackage p\n\nvar X = 1\n")
	if err := checkEquivalent(before, after); err != nil {
		t.Fatalf("checkEquivalent() = %v, want nil", err)
	}
}

func TestScanRecognizesConstraintsAfterBOM(t *testing.T) {
	src := "\ufeff//go:build linux\n// +build linux\n\npackage p\n\nvar X = 1\n"
	if got := scan([]byte(src)); len(got) != 0 {
		t.Fatalf("scan() = %d spans, want 0", len(got))
	}
}

func TestStripKeepsLegacyConstraintAfterBOM(t *testing.T) {
	src := "\ufeff// +build linux\n\npackage p\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "// +build linux") {
		t.Fatalf("strip() = %q, want the legacy constraint kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesLineDirective(t *testing.T) {
	src := "package p\n\n/*line generated.go:900*/var X = 1\n\n// gone\nvar Y = 2\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a file that maps positions with line directives")
	}
}

func TestStripKeepsGeneratedMarker(t *testing.T) {
	src := "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage p\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "DO NOT EDIT.") {
		t.Fatalf("strip() = %q, want the generated marker kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripKeepsDirectiveGroupedWithDoc(t *testing.T) {
	src := "package p\n\n// helper docs\n//go:noinline\nfunc F() {}\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "//go:noinline") {
		t.Fatalf("strip() = %q, want the directive kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesWhenReprintFails(t *testing.T) {
	saved := printNode
	printNode = func(io.Writer, *token.FileSet, any) error { return errors.New("boom") }
	defer func() { printNode = saved }()
	if _, err := strip([]byte("package p\n\n// gone\nvar X = 1\n")); err == nil {
		t.Fatal("strip() = nil, want the reprint error")
	}
}

func TestStripRefusesActivatedConstraint(t *testing.T) {
	src := "package p\n\n//go:build linux\n\n// gone\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that would relocate a build constraint into the header")
	}
}

func TestStripRefusesMovedDirective(t *testing.T) {
	src := "package p\n\n// gone\n\n//go:generate echo hi\n\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that would move a //go:generate directive's line")
	}
}

func TestStripKeepsExampleOutput(t *testing.T) {
	src := "package p\n\nfunc Example() {\n\tprintln(\"hi\")\n\n\t// Output:\n\t// hi\n}\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "Output:") || !strings.Contains(got, "// hi") {
		t.Fatalf("strip() = %q, want the example output comment and its body kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesExampleActivation(t *testing.T) {
	src := "package p\n\nfunc ExampleFoo() {\n\tprintln(\"hi\")\n\n\t// Output:\n\t// hi\n\n\t// trailing note\n}\n\n// gone\nvar X = 1\n"
	premise := func(name, want string) {
		f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		examples := doc.Examples(f)
		if len(examples) != 1 || examples[0].Output != want {
			t.Fatalf("premise: go/doc must classify the example as %s, got %+v", name, examples)
		}
	}
	premise("compiled-only", "")
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that would make a compiled-only example run")
	}
}

func TestStripKeepsExampleClassification(t *testing.T) {
	src := "package p\n\nfunc ExampleFoo() {\n\tprintln(\"hi\")\n\n\t// Output:\n\t// hi\n}\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	before, err := exampleSet([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	after, err := exampleSet(out)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("example classification changed:\nbefore %q\nafter  %q", before, after)
	}
	if strings.Contains(string(out), "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", out)
	}
}

func TestStripKeepsCgoPreambleDropsOthers(t *testing.T) {
	src := "package p\n\n/*\n#include <stdio.h>\n*/\nimport \"C\"\n\n// fmt doc\nimport \"fmt\"\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "#include <stdio.h>") {
		t.Fatalf("strip() = %q, want the cgo preamble kept", got)
	}
	if strings.Contains(got, "fmt doc") {
		t.Fatalf("strip() = %q, want the non-cgo import doc gone", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripKeepsGenerateInsideBlockComment(t *testing.T) {
	src := "package p\n\n/*\n//go:generate echo hi\n*/\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "//go:generate echo hi") {
		t.Fatalf("strip() = %q, want the embedded directive kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesRelocatedDirective(t *testing.T) {
	src := "package p\n\nfunc f() {\n\tif true {\n//go:generate echo hi\n\t\t_ = 1\n\t}\n}\n\n// gone\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a file whose column-zero directive would relocate")
	}
}

func TestStripRefusesActivatedDirective(t *testing.T) {
	src := "//go:build linux\n\t//go:generate echo hi\n\npackage p\n\n// gone\nvar X = 1\n"
	if len(generateCommands([]byte(src))) != 0 {
		t.Fatal("premise: an indented directive is dormant, so the before set must be empty")
	}
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that would reprint an indented directive at column zero")
	}
}

func TestStripRefusesUnterminatedDirective(t *testing.T) {
	src := "package p\n\n// gone\nvar X = 1\n//go:generate echo hi"
	before := generateCommands([]byte(src))
	if len(before) != 1 || !before[0].eof {
		t.Fatalf("premise: the final line must be a directive without a newline, got %v", before)
	}
	_, err := strip([]byte(src))
	if err == nil {
		t.Fatal("strip() accepted a rewrite that would terminate a final //go:generate line that go generate refuses to run")
	}
	if !strings.Contains(err.Error(), "//go:generate") {
		t.Fatalf("strip() = %v, want the generate guard to fire", err)
	}
}

func TestStripKeepsTerminatedDirectiveAtEOF(t *testing.T) {
	src := "package p\n\n// gone\nvar X = 1\n//go:generate echo hi\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasSuffix(got, "\n//go:generate echo hi\n") {
		t.Fatalf("strip() = %q, want the terminated directive kept as the final line", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripKeepsTopLevelGenerate(t *testing.T) {
	src := "package p\n\n//go:generate echo hi\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "\n//go:generate echo hi\n") {
		t.Fatalf("strip() = %q, want the directive kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripKeepsGeneratedMarkerInBlockComment(t *testing.T) {
	src := "/*\n// Code generated by gen. DO NOT EDIT.\n*/\n\npackage p\n\n// gone\nvar X = 1\n"
	oracle, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !ast.IsGenerated(oracle) {
		t.Fatal("premise: go/ast must treat this source as generated")
	}
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "DO NOT EDIT.") {
		t.Fatalf("strip() = %q, want the generated marker kept", got)
	}
}

func TestStripKeepsCanonicalImportComment(t *testing.T) {
	for _, head := range []string{
		"package p // import \"example.com/canonical\"\n",
		"package p //import \"example.com/canonical\"\n",
		"package p //  import \"example.com/canonical\"\n",
		"package p /*import \"example.com/canonical\"*/\n",
		"package p //import`example.com/canonical`\n",
	} {
		src := head + "\n// gone\nvar X = 1\n"
		out, err := strip([]byte(src))
		if err != nil {
			t.Fatalf("strip(%q) = %v", head, err)
		}
		got := string(out)
		if !strings.Contains(got, "example.com/canonical") {
			t.Fatalf("strip(%q) = %q, want the canonical import comment kept", head, got)
		}
		if strings.Contains(got, "// gone") {
			t.Fatalf("strip(%q) = %q, want the ordinary comment gone", head, got)
		}
	}
}

func TestStripDropsDocsOnEachNodeKind(t *testing.T) {
	src := "package p\n\n// T doc\ntype T struct {\n\t// field doc\n\tF int // trailing\n}\n\n// C doc\nconst C = 1\n\n// V doc\nvar V = 2\n\n// F doc\nfunc F() {}\n\n// G doc\nfunc (T) G() {}\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, gone := range []string{"T doc", "field doc", "trailing", "C doc", "V doc", "F doc", "G doc"} {
		if strings.Contains(got, gone) {
			t.Fatalf("strip() = %q, want %q gone", got, gone)
		}
	}
	for _, kept := range []string{"type T struct", "F int", "const C = 1", "var V = 2", "func F()", "func (T) G()"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("strip() = %q, want %q kept", got, kept)
		}
	}
}

func TestStripRefusesMultipleLegacyConstraintRewrite(t *testing.T) {
	for _, tags := range []string{"// +build linux\n// +build amd64\n", "// +build amd64\n// +build linux\n", "// +build linux\n// +build amd64\n// +build arm64\n"} {
		src := tags + "\npackage p\n\n// c\nvar X = 1\n"
		if _, err := strip([]byte(src)); err == nil {
			t.Fatalf("strip(%q) = nil error, want refusal: the reprint merges the legacy lines into one constraint comment", src)
		}
	}
}

func TestStripFilesRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, filepath.Join(dir, "target.go"), "package p\n\n// gone\nvar X = 1\n")
	link := filepath.Join(dir, "link.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := stripFiles([]string{link}); got != 2 {
		t.Fatalf("stripFiles() = %d, want 2", got)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "// gone") {
		t.Fatalf("stripFiles() wrote through the symlink: %q", body)
	}
}

func TestStripFilesWritesNothingOnError(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, filepath.Join(dir, "a.go"), "package p\n\n// gone\nvar X = 1\n")
	bad := writeFile(t, filepath.Join(dir, "b.go"), "package p\n\n// gone\nfunc (\n")
	if got := stripFiles([]string{good, bad}); got != 2 {
		t.Fatalf("stripFiles() = %d, want 2", got)
	}
	body, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "// gone") {
		t.Fatalf("stripFiles() rewrote %s despite the error: %q", good, body)
	}
}

func TestStripFilesWritesRegularFile(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "regular.go"), "package p\n\n// gone\nvar X = 1\n")
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "// gone") || !strings.Contains(string(body), "var X = 1") {
		t.Fatalf("stripFiles() = %q, want the comment gone and the declaration kept", body)
	}
}

func TestStripFilesPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "a.go"), "package p\n\n// gone\nvar X = 1\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "// gone") {
		t.Fatalf("stripFiles() = %q, want the comment gone", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("stripFiles left %d entries, want 1", len(entries))
	}
}

func TestStripFilesSwapsByRename(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "a.go"), "package p\n\n// gone\nvar X = 1\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("stripFiles rewrote the file in place; want a rename swap")
	}
}

func TestStripFilesLeavesNoTargetsRewrittenOnWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the write cannot be made to fail")
	}
	dir := t.TempDir()
	writable := filepath.Join(dir, "writable")
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(writable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	first := writeFile(t, filepath.Join(writable, "a.go"), "package p\n\n// gone\nvar X = 1\n")
	second := writeFile(t, filepath.Join(locked, "b.go"), "package p\n\n// gone\nvar Y = 2\n")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	if got := stripFiles([]string{first, second}); got != 2 {
		t.Fatalf("stripFiles() = %d, want 2", got)
	}
	for _, path := range []string{first, second} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "// gone") {
			t.Fatalf("stripFiles() rewrote %s after a later replacement failed: %q", path, body)
		}
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(writable)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("stripFiles left %d entries in the writable directory, want 1", len(entries))
	}
}

func TestStageFileCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	if _, err := stageFile(filepath.Join(dir, "missing", "a.go"), []byte("x\n"), 0o644); err == nil {
		t.Fatal("stageFile() = nil, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stageFile left %d entries after a failure, want 0", len(entries))
	}
}

func TestGenerateCommandsMatchesTabSeparator(t *testing.T) {
	src := "package p\n\n//go:generate\tstringer -type=T\n\nvar X = 1\n"
	if got := generateCommands([]byte(src)); len(got) != 1 {
		t.Fatalf("generateCommands() = %d commands, want 1 (tab separator)", len(got))
	}
}

func TestStripRefusesMovedTabSeparatedGenerate(t *testing.T) {
	src := "package p\n\n// gone\n\n//go:generate\tstringer -type=T\n\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that would move a tab-separated //go:generate directive")
	}
}

func TestStripKeepsTabGenerateInsideBlockComment(t *testing.T) {
	src := "package p\n\n/*\n//go:generate\tsetenv\n*/\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); !strings.Contains(got, "//go:generate\tsetenv") {
		t.Fatalf("strip() = %q, want the embedded tab-separated directive kept", got)
	}
}

func TestStripDropsNearMissGeneratedMarker(t *testing.T) {
	src := "package p\n\n// Code generated by hand; edit freely.\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "edit freely") {
		t.Fatalf("strip() = %q, want the near-miss generated marker dropped", got)
	}
}

func TestStripKeepsUnorderedExampleOutput(t *testing.T) {
	src := "package p\n\nfunc Example() {\n\tprintln(2)\n\tprintln(1)\n\n\t// Unordered output:\n\t// 1\n\t// 2\n}\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "// Unordered output:") || !strings.Contains(got, "// 1") {
		t.Fatalf("strip() = %q, want the unordered example output kept", got)
	}
	if !strings.Contains(got, "// 2") {
		t.Fatalf("strip() = %q, want the whole expected-output block kept", got)
	}
}

func TestStripDropsOutputMentionMidComment(t *testing.T) {
	src := "package p\n\n// the expected output: details below\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "details below") {
		t.Fatalf("strip() = %q, want a comment merely mentioning output: dropped", got)
	}
}

func TestLoadBearingRejectsUppercaseLookalike(t *testing.T) {
	for _, text := range []string{"//GO:BUILD", "//GO:NOSPLIT"} {
		if loadBearing([]byte(text), span{start: 0, end: len(text)}) {
			t.Fatalf("loadBearing(%q) = true, want false", text)
		}
	}
}

func TestStripDropsSpecLevelAndPackageDocs(t *testing.T) {
	src := "// Package p demonstrates docs.\npackage p\n\nimport (\n\t// fmt doc\n\t\"fmt\"\n)\n\nconst (\n\t// C doc\n\tC = 1\n)\n\ntype (\n\t// T doc\n\tT struct{}\n)\n\nvar (\n\t// V doc\n\tV int\n)\n\nvar _ = fmt.Sprint\nvar _ = C\nvar _ T\nvar _ = V\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, gone := range []string{"Package p demonstrates", "fmt doc", "C doc", "T doc", "V doc"} {
		if strings.Contains(got, gone) {
			t.Fatalf("strip() = %q, want %q gone", got, gone)
		}
	}
}

func TestStripKeepsCgoPreambleOnParenthesizedImport(t *testing.T) {
	src := "package p\n\nimport (\n\t/*\n\t\t#include <stdio.h>\n\t*/\n\t\"C\"\n)\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "#include <stdio.h>") {
		t.Fatalf("strip() = %q, want the cgo preamble on the import spec kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesIndentedLineDirective(t *testing.T) {
	src := "package p\n\nvar X = 1\n\n\t/*line generated.go:900*/\nvar Y = 2\n\n// gone\nvar Z = 3\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a file whose indented /*line*/ remaps positions")
	}
}

func TestStripDropsLineLookalikesTheScannerIgnores(t *testing.T) {
	for name, src := range map[string]string{
		"tab separator":  "//line\tgenerated.go:900\npackage p\n\n// gone\nvar X = 1\n",
		"trailing prose": "package p\n\nfunc f() error { return nil } //line by line, tallying\n\n// gone\nvar X = 1\n",
		"indented":       "package p\n\nfunc f() {\n\t//line generated.go:900\n\t_ = 1\n}\n\n// gone\nvar X = 1\n",
	} {
		out, err := strip([]byte(src))
		if err != nil {
			t.Fatalf("strip(%s) = %v, want acceptance: the scanner ignores this //line form", name, err)
		}
		if strings.Contains(string(out), "// gone") {
			t.Fatalf("strip(%s) = %q, want the ordinary comment gone", name, out)
		}
	}
}

func TestStripRefusesColumnZeroLineDirective(t *testing.T) {
	src := "//line generated.go:900\npackage p\n\n// gone\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a file whose column-1 //line directive remaps positions")
	}
}

func TestStripFilesPreservesSetuidMode(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "suid.go"), "package p\n\n// gone\nvar X = 1\n")
	if err := os.Chmod(path, os.FileMode(0o755)|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("mode = %v, want the original setuid mode restored across chown", info.Mode())
	}
	if got := string(mustRead(t, path)); strings.Contains(got, "// gone") {
		t.Fatalf("stripFiles() = %q, want the comment gone", got)
	}
}

func TestStripLeavesAllKeptFileByteIdentical(t *testing.T) {
	src := []byte("//go:build linux\npackage p\nvar X   =   1\n")
	out, err := strip(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("strip() = %q, want the input byte-identical", out)
	}
}

func TestStripFilesLeavesCleanFileUnrenamed(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "clean.go"), "package p\n\nvar X = 1\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("stripFiles() renamed a clean file; want it untouched")
	}
}

func TestScanExtendsUnterminatedBlockToEOF(t *testing.T) {
	src := []byte("package p\n\n/* unterminated")
	got := scan(src)
	if len(got) != 1 {
		t.Fatalf("scan() = %d spans, want 1", len(got))
	}
	if lit := string(src[got[0].start:got[0].end]); lit != "/* unterminated" {
		t.Fatalf("span = %q, want the whole tail", lit)
	}
}

func TestStripDropsBOMWhenReprinting(t *testing.T) {
	src := append(append([]byte{}, bom...), []byte("package p\n\n// gone\nvar X = 1\n")...)
	out, err := strip(src)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(out, bom) {
		t.Fatalf("strip() kept the BOM: %q", out)
	}
	if !bytes.Contains(out, []byte("var X = 1")) {
		t.Fatalf("strip() = %q, want the declaration kept", out)
	}
}

func TestStripConvertsCRLFToLF(t *testing.T) {
	out, err := strip([]byte("package p\r\n\r\n// gone\r\nvar X = 1\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "\r") {
		t.Fatalf("strip() = %q, want LF-only output", got)
	}
}

func TestStripKeepsTrailingCommentOnImportC(t *testing.T) {
	src := "package p\n\nimport \"C\" // note\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `import "C" // note`) {
		t.Fatalf("strip() = %q, want the trailing comment on import \"C\" kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripFilesPreservesOwner(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "owned.go"), "package p\n\n// gone\nvar X = 1\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fileOwner(before)
	if !ok {
		t.Skip("ownership not observable on this platform")
	}
	if got := stripFiles([]string{path}); got != 0 {
		t.Fatalf("stripFiles() = %d, want 0", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gotUID, gotGID, _ := fileOwner(after)
	if gotUID != uid || gotGID != gid {
		t.Fatalf("owner changed from %d:%d to %d:%d across the rewrite", uid, gid, gotUID, gotGID)
	}
	if got := string(mustRead(t, path)); strings.Contains(got, "// gone") {
		t.Fatalf("stripFiles() = %q, want the comment gone", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStripRefusesHoistedConstraint(t *testing.T) {
	src := "//go:build linux\n\npackage main\n\n// gone\n\n//go:build plan9\nfunc f() {}\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that hoists a kept misplaced //go:build next to the header one, leaving the file unloadable")
	}
}

func TestStripRefusesConstraintLineRewrite(t *testing.T) {
	src := "//go:build linux\n\npackage main\n\n// gone\n\n//note:x\n// +build plan9\nfunc f() {}\n"
	_, err := strip([]byte(src))
	if err == nil {
		t.Fatal("strip() accepted a rewrite that deletes a kept // +build line and fabricates one in the header")
	}
	if !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("strip() = %v, want the constraint guard to fire", err)
	}
}

func TestStripKeepsKeptLineCommentVerbatim(t *testing.T) {
	src := "package p\n\n//go:noinline\n// explanation the keep rules preserve\nfunc f() {}\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "//go:noinline\n") || !strings.Contains(got, "// explanation the keep rules preserve\n") {
		t.Fatalf("strip() = %q, want every kept comment line to survive the reprint", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripAcceptsCRLFGenerate(t *testing.T) {
	src := "package p\r\n\r\n//go:generate echo hi\r\n\r\n// gone\r\nvar X = 1\r\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatalf("strip() = %v, want acceptance: go generate ignores the carriage return", err)
	}
	got := string(out)
	if !strings.Contains(got, "//go:generate echo hi") {
		t.Fatalf("strip() = %q, want the directive kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesMovedCgoPreamble(t *testing.T) {
	src := "package p\n\n// gone\n\n/*\nenum { L = __LINE__ };\n*/\nimport \"C\"\n\nvar X = 1\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that moves the cgo preamble: __LINE__ inside it tracks the .go source line")
	}
}

func TestStripKeepsCgoPreamblePosition(t *testing.T) {
	src := "package p\n\n/*\nenum { L = __LINE__ };\n*/\nimport \"C\"\n\n// gone\nvar X = 1\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "__LINE__") {
		t.Fatalf("strip() = %q, want the preamble kept", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}

func TestStripRefusesBOMActivatedLineDirective(t *testing.T) {
	src := append(append([]byte{}, bom...), []byte("//line mapped.go:100\npackage p\n\n// gone\nvar X = 1\n")...)
	fset := token.NewFileSet()
	premise, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if pos := fset.Position(premise.Package); pos.Filename != "" || pos.Line != 2 {
		t.Fatalf("premise: the scanner must not honor a BOM-prefixed //line, got %v", pos)
	}
	if _, err := strip(src); err == nil {
		t.Fatal("strip() accepted a rewrite that would activate the BOM-shadowed //line directive")
	}
}

func TestStripRefusesShiftedPreambleInGroup(t *testing.T) {
	src := "package main\n\n// Code generated by x. DO NOT EDIT.\nimport (\n\t// gone\n\n\t/*\n\tenum { L = __LINE__ };\n\t*/\n\t\"C\"\n)\n\nimport \"fmt\"\n\nfunc main() { _ = fmt.Sprint }\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that shifts the cgo preamble while the declaration doc holds the minimum line")
	}
}

func TestStripRefusesReindentedCgoPreamble(t *testing.T) {
	src := "package main\n\nimport (\n/*\n#define STR \"a\\\nb\"\n*/\n\"C\"\n)\n\nimport \"fmt\"\n\n// gone\nfunc main() { _ = fmt.Sprint }\n"
	if _, err := strip([]byte(src)); err == nil {
		t.Fatal("strip() accepted a rewrite that reindents the cgo preamble: a backslash continuation embeds the new indentation in the C text")
	}
}

func TestStripKeepsIndentedCgoPreambleBytes(t *testing.T) {
	src := "package main\n\nimport (\n\t/*\n\t\t#define STR \"a\\\tb\"\n\t*/\n\t\"C\"\n)\n\nimport \"fmt\"\n\n// gone\nfunc main() { _ = fmt.Sprint }\n"
	out, err := strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "\t\t#define STR \"a\\\tb\"\n") {
		t.Fatalf("strip() = %q, want the preamble bytes unchanged", got)
	}
	if strings.Contains(got, "// gone") {
		t.Fatalf("strip() = %q, want the ordinary comment gone", got)
	}
}
