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
	goDirPattern  = regexp.MustCompile(`^//go:[a-z][a-z0-9_]*([ \t].*)?\r?$`)
	linePattern   = regexp.MustCompile(`^//line([ \t].*)?\r?$`)
	exportPattern = regexp.MustCompile(`^//export([ \t].*)?\r?$`)
	nolintPattern = regexp.MustCompile(`^//nolint(:[0-9A-Za-z_,-]+([ \t].*)?)?\r?$`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	blankRun      = regexp.MustCompile(`\n{3,}`)
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
				for _, cg := range []*ast.CommentGroup{gd.Doc, imp.Doc} {
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
			atLineStart := fset.Position(c.Pos()).Column == 1
			if !isExempt(text, atLineStart, isDoc) {
				s.comments = append(s.comments, span{start, end})
			}
		}
	}
	return s, nil
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
	if atLineStart && (goDirPattern.Match(text) || linePattern.Match(text)) {
		return true
	}
	if isDoc && (goDirPattern.Match(text) || exportPattern.Match(text)) {
		return true
	}
	return nolintPattern.Match(text)
}

func blank(b []byte) bool { return len(bytes.Trim(b, " \t\r")) == 0 }

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func tidy(b []byte) []byte {
	return blankRun.ReplaceAll(trailingSpace.ReplaceAll(b, []byte("\n")), []byte("\n\n"))
}

func stripComments(src []byte, s *scan) []byte {
	if len(s.comments) == 0 {
		return src
	}
	rs := make([]span, len(s.comments))
	for k, c := range s.comments {
		rs[k] = expandRange(src, c)
	}
	sort.Slice(rs, func(a, b int) bool { return rs[a].start < rs[b].start })
	var removals []span
	for _, r := range rs {
		if k := len(removals); k > 0 && r.start <= removals[k-1].end {
			removals[k-1].end = max(removals[k-1].end, r.end)
			continue
		}
		removals = append(removals, r)
	}
	var out []byte
	prev := 0
	for _, r := range removals {
		out = append(out, src[prev:r.start]...)
		if bytes.IndexByte(src[r.start:r.end], '\n') >= 0 {
			if (len(out) == 0 || out[len(out)-1] != '\n') && (r.end >= len(src) || src[r.end] != '\n') {
				out = append(out, '\n')
			}
		} else if len(out) > 0 && r.end < len(src) && !isSpace(out[len(out)-1]) && !isSpace(src[r.end]) {
			out = append(out, ' ')
		}
		prev = r.end
	}
	out = append(out, src[prev:]...)
	out = tidyOutsideLiterals(out)
	out = bytes.TrimLeft(out, "\n")
	out = bytes.TrimRight(out, "\n")
	if len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}

func expandRange(src []byte, r span) span {
	lineStart := bytes.LastIndexByte(src[:r.start], '\n') + 1
	nl := bytes.IndexByte(src[r.end:], '\n')
	if nl < 0 {
		nl = len(src) - r.end
	}
	lineEnd := r.end + nl
	if !blank(src[lineStart:r.start]) || !blank(src[r.end:lineEnd]) {
		e := r.end
		for e < len(src) && (src[e] == ' ' || src[e] == '\t') {
			e++
		}
		return span{r.start, e}
	}
	end := lineEnd
	if end < len(src) {
		end++
	}
	return span{lineStart, end}
}

func tidyOutsideLiterals(src []byte) []byte {
	fset, f, err := parseGo(src)
	if err != nil {
		return src
	}
	tf := fset.File(f.Package)
	var literals []span
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok {
			literals = append(literals, span{tf.Offset(lit.Pos()), tf.Offset(lit.End())})
		}
		return true
	})
	var out []byte
	prev := 0
	for _, l := range literals {
		if l.start > prev {
			out = append(out, tidy(src[prev:l.start])...)
		}
		out = append(out, src[l.start:l.end]...)
		prev = l.end
	}
	if prev < len(src) {
		out = append(out, tidy(src[prev:])...)
	}
	return out
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

func listGoFiles(wide bool) ([]string, error) {
	args := []string{"ls-files"}
	if wide {
		args = append(args, "--cached", "--others", "--exclude-standard")
	}
	args = append(args, "-z", "--", "*.go")
	out, err := exec.Command("git", args...).Output()
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
	mode := "write"
	for _, a := range os.Args[1:] {
		switch a {
		case "--check", "--stats":
			mode = a[2:]
		default:
			die(fmt.Errorf("unknown flag %q", a))
		}
	}
	files, err := listGoFiles(mode == "write")
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
		switch mode {
		case "stats":
			if n > 0 {
				fmt.Printf("%s:%d\n", f, n)
			}
		case "check":
			if msg := judge(f, n, allow); msg != "" {
				fmt.Println(msg)
				bad++
			}
		default:
			if n == 0 {
				continue
			}
			if err := os.WriteFile(f, stripComments(src, s), 0o644); err != nil {
				die(err)
			}
			fmt.Printf("stripped %s (%d comments)\n", f, n)
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
