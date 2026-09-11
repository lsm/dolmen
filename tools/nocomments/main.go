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
)

const allowlistPath = "scripts/no-comments-allowlist.txt"

type span struct{ start, end int }

type scan struct{ comments []span }

var (
	goDirPattern     = regexp.MustCompile(`^//go:[a-z][a-z0-9_]*([ \t].*)?\r?$`)
	legacyBuildLine  = regexp.MustCompile(`^//[ \t]*\+build([ \t].*)?\r?$`)
	linePattern      = regexp.MustCompile(`^//line .*:\d+(?::\d+)? ?\r?$`)
	blockLinePattern = regexp.MustCompile(`^/\*line .*:\d+(?::\d+)? ?\*/\r?$`)
	docDirPattern    = regexp.MustCompile(`^//(go:(embed|linkname|noinline|nosplit|norace|nocheckptr|noescape|uintptrescapes|wasmimport)|export)([ \t].*)?\r?$`)
	nolintPattern    = regexp.MustCompile(`^//nolint(:[0-9A-Za-z_,-]+([ \t].*)?)?\r?$`)
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

func isExempt(text []byte, atLineStart, isDoc bool) bool {
	if atLineStart && (goDirPattern.Match(text) || legacyBuildLine.Match(text) || linePattern.Match(text)) {
		return true
	}
	if isDoc && docDirPattern.Match(text) {
		return true
	}
	return blockLinePattern.Match(text) || nolintPattern.Match(text)
}

func scanSource(src []byte) (*scan, error) {
	fset, f, err := parseGo(src)
	if err != nil {
		return nil, fmt.Errorf("unparseable: %w", err)
	}
	tf := fset.File(f.Package)
	preambles := cgoPreambles(f)
	docs := docGroups(f)
	s := &scan{}
	for _, g := range f.Comments {
		if preambles[g.Pos()] {
			continue
		}
		isDoc := docs[g.Pos()]
		for _, c := range g.List {
			start := tf.Offset(c.Pos())
			end := commentEnd(src, start)
			text := src[start:end]
			atLineStart := fset.PositionFor(c.Pos(), false).Column == 1
			if !isExempt(text, atLineStart, isDoc) {
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
		s, err := scanSource(src)
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
