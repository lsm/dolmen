package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const allowlistPath = "scripts/no-comments-allowlist.txt"

type span struct{ start, end int }

type scan struct{ comments []span }

var (
	goDirPattern     = regexp.MustCompile(`^//go:[a-z][a-z0-9_]*([ \t].*)?\r?$`)
	buildTagPattern  = regexp.MustCompile(`^//go:build([ \t].*)?\r?$`)
	legacyBuildLine  = regexp.MustCompile(`^//[ \t]*\+build([ \t].*)?\r?$`)
	linePattern      = regexp.MustCompile(`^//line .*:\d+(?::\d+)? ?\r?$`)
	blockLinePattern = regexp.MustCompile(`^/\*line .*:\d+(?::\d+)? ?\*/\r?$`)
	lineScannedOnly  = regexp.MustCompile(`^//go:(build|generate|line|debug)([ \t].*)?\r?$`)
	exportPattern    = regexp.MustCompile(`^//export([ \t].*)?\r?$`)
	nolintPattern    = regexp.MustCompile(`^//nolint(:[0-9A-Za-z_,-]+)?([ \t].*)?\r?$`)
	outputPattern    = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)
)

func die(err error) {
	fmt.Fprintln(os.Stderr, "nocomments:", err)
	os.Exit(2)
}

func parseGo(src []byte) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	return fset, f, nil
}

func cgoPreambles(f *ast.File) map[token.Pos]bool {
	exempt := map[token.Pos]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gd.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok {
				continue
			}
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "C" {
				docs := []*ast.CommentGroup{imp.Doc}
				if len(gd.Specs) == 1 {
					docs = append(docs, gd.Doc)
				}
				for _, cg := range docs {
					if cg != nil {
						exempt[cg.Pos()] = true
					}
				}
			}
		}
	}
	return exempt
}

func isTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

func exampleOutputs(f *ast.File) map[token.Pos]bool {
	exempt := map[token.Pos]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil || !isTestName(fd.Name.Name, "Example") {
			continue
		}
		if p := fd.Type.Params; len(p.List) != 0 {
			continue
		}
		if r := fd.Type.Results; r != nil && len(r.List) != 0 {
			continue
		}
		var last *ast.CommentGroup
		for _, g := range f.Comments {
			if g.Pos() > fd.Body.Lbrace && g.End() < fd.Body.Rbrace {
				last = g
			}
		}
		if last == nil {
			continue
		}
		if outputPattern.MatchString(last.Text()) {
			exempt[last.Pos()] = true
		}
	}
	return exempt
}

func commentEnd(src []byte, start int) int {
	if bytes.HasPrefix(src[start:], []byte("//")) {
		if e := bytes.IndexByte(src[start:], '\n'); e >= 0 {
			return start + e
		}
		return len(src)
	}
	if rel := bytes.Index(src[start+2:], []byte("*/")); rel >= 0 {
		return start + rel + 4
	}
	return len(src)
}

func docGroups(f *ast.File) map[token.Pos]bool {
	docs := map[token.Pos]bool{}
	mark := func(cg *ast.CommentGroup) {
		if cg != nil {
			docs[cg.Pos()] = true
		}
	}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			mark(decl.Doc)
			for _, s := range decl.Specs {
				switch sp := s.(type) {
				case *ast.ImportSpec:
					mark(sp.Doc)
				case *ast.ValueSpec:
					mark(sp.Doc)
				case *ast.TypeSpec:
					mark(sp.Doc)
				}
			}
		case *ast.FuncDecl:
			mark(decl.Doc)
		}
	}
	return docs
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func atLineStart(src []byte, start int) bool {
	return start == 0 || src[start-1] == '\n'
}

func afterFileBOM(src []byte, start int) bool {
	return bytes.HasPrefix(src, utf8BOM) && start == len(utf8BOM)
}

func isExempt(text []byte, atLineStart, afterBOM, isDoc bool) bool {
	if (atLineStart || afterBOM) && (buildTagPattern.Match(text) || legacyBuildLine.Match(text)) {
		return true
	}
	if atLineStart && (goDirPattern.Match(text) || linePattern.Match(text)) {
		return true
	}
	if isDoc && (goDirPattern.Match(text) || exportPattern.Match(text)) && !lineScannedOnly.Match(text) {
		return true
	}
	return blockLinePattern.Match(text) || nolintPattern.Match(text)
}

func isGeneratedMarker(text []byte) bool {
	for _, line := range bytes.Split(text, []byte("\n")) {
		rest, ok := bytes.CutPrefix(line, []byte("// Code generated "))
		if !ok {
			continue
		}
		if _, ok := bytes.CutSuffix(rest, []byte(" DO NOT EDIT.")); ok {
			return true
		}
	}
	return false
}

func scanSource(src []byte, path string) (*scan, error) {
	fset, f, err := parseGo(src)
	if err != nil {
		return nil, fmt.Errorf("unparseable: %w", err)
	}
	tf := fset.File(f.Package)
	preambles := cgoPreambles(f)
	var outputs map[token.Pos]bool
	if strings.HasSuffix(path, "_test.go") {
		outputs = exampleOutputs(f)
	}
	docs := docGroups(f)
	s := &scan{}
	for _, g := range f.Comments {
		if preambles[g.Pos()] || outputs[g.Pos()] {
			continue
		}
		isDoc := docs[g.Pos()]
		for _, c := range g.List {
			start := tf.Offset(c.Pos())
			end := commentEnd(src, start)
			text := src[start:end]
			if c.Pos() < f.Package && isGeneratedMarker(text) {
				continue
			}
			if !isExempt(text, atLineStart(src, start), afterFileBOM(src, start), isDoc) {
				s.comments = append(s.comments, span{start, end})
			}
		}
	}
	return s, nil
}

func judge(file string, count int, allow map[string]int) string {
	if count == 0 {
		return ""
	}
	if base, ok := allow[file]; !ok {
		return fmt.Sprintf("%s: %d comments in non-allowlisted file", file, count)
	} else if count > base {
		return fmt.Sprintf("%s: comments increased %d -> %d", file, base, count)
	}
	return ""
}

func readAllowlist(path string) (map[string]int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	allow := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k := strings.LastIndexByte(line, ':')
		if k < 0 {
			return nil, fmt.Errorf("%s: malformed line %q", path, line)
		}
		v, err := strconv.Atoi(line[k+1:])
		if err != nil {
			return nil, fmt.Errorf("%s: malformed line %q", path, line)
		}
		allow[line[:k]] = v
	}
	return allow, nil
}

func listGoFiles() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z", "--", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	files := []string{}
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	sort.Strings(files)
	return files, nil
}

func main() {
	mode := ""
	for _, a := range os.Args[1:] {
		switch a {
		case "--check", "--stats":
			mode = a[2:]
		default:
			die(fmt.Errorf("unknown flag %q", a))
		}
	}
	if mode == "" {
		die(fmt.Errorf("usage: nocomments --check | --stats"))
	}
	files, err := listGoFiles()
	if err != nil {
		die(err)
	}
	allow := map[string]int{}
	if mode == "check" {
		if allow, err = readAllowlist(allowlistPath); err != nil {
			die(err)
		}
	}
	bad := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			die(err)
		}
		s, err := scanSource(src, f)
		if err != nil {
			die(fmt.Errorf("cannot parse %s: %w", f, err))
		}
		n := len(s.comments)
		if mode == "stats" {
			if n > 0 {
				fmt.Printf("%s:%d\n", f, n)
			}
			continue
		}
		if msg := judge(f, n, allow); msg != "" {
			fmt.Println(msg)
			bad++
		}
	}
	if mode == "check" {
		if bad > 0 {
			fmt.Printf("no-comments: %d violation(s)\n", bad)
			os.Exit(1)
		}
		fmt.Println("no-comments: ok")
	}
}
