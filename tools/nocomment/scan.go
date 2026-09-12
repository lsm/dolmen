package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/doc"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

type span struct {
	start int
	end   int
}

type directive struct {
	name       string
	space      bool
	header     bool
	legacy     bool
	line       bool
	columnZero bool
	separators string
	bare       bool
}

type comment struct {
	span span
	name string
	load bool
}

var directives = []directive{
	{name: "+build", space: true, header: true, legacy: true, line: true, bare: true},
	{name: "go:build", header: true, line: true, bare: true},
	{name: "go:embed", line: true, separators: " ", bare: true},
	{name: "go:generate", columnZero: true, separators: " \t"},
}

var bom = []byte{0xef, 0xbb, 0xbf}

func directiveName(src []byte, sp span, header bool, buildEnd int) string {
	text := string(src[sp.start:sp.end])
	if !strings.HasPrefix(text, "//") {
		return ""
	}
	body := text[2:]
	for _, d := range directives {
		rest := body
		if d.space {
			rest = strings.TrimSpace(rest)
		}
		if !strings.HasPrefix(rest, d.name) || !boundary(rest[len(d.name):], d) {
			continue
		}
		if d.header && !header {
			continue
		}
		if d.line && !atLineStart(src, sp.start) {
			continue
		}
		if d.columnZero && !atColumnZero(src, sp.start) {
			continue
		}
		if d.legacy && sp.start >= buildEnd {
			continue
		}
		return d.name
	}
	return ""
}

func loadBearing(src []byte, sp span) bool {
	text := string(src[sp.start:sp.end])
	if strings.HasPrefix(text, "/*") {
		return strings.HasPrefix(text[2:], "line ")
	}
	if !strings.HasPrefix(text, "//") {
		return false
	}
	body := text[2:]
	if strings.HasPrefix(body, "line ") || strings.HasPrefix(body, "extern ") || strings.HasPrefix(body, "export ") {
		return true
	}
	colon := strings.IndexByte(body, ':')
	if colon <= 0 || colon+1 >= len(body) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := body[i]
		if !('a' <= b && b <= 'z' || '0' <= b && b <= '9') {
			return false
		}
	}
	return true
}

func lineDirective(src []byte) bool {
	for _, c := range comments(src) {
		text := string(src[c.span.start:c.span.end])
		if strings.HasPrefix(text, "/*") {
			if strings.HasPrefix(text[2:], "line ") {
				return true
			}
			continue
		}
		if strings.HasPrefix(text, "//") && atColumnZero(src, c.span.start) && strings.HasPrefix(text[2:], "line ") {
			return true
		}
	}
	return false
}

func boundary(rest string, d directive) bool {
	if rest == "" {
		return d.bare
	}
	if d.separators == "" {
		r, _ := utf8.DecodeRuneInString(rest)
		return unicode.IsSpace(r)
	}
	return strings.IndexByte(d.separators, rest[0]) >= 0
}

func atLineStart(src []byte, start int) bool {
	lineStart := start
	for lineStart > 0 && src[lineStart-1] != '\n' {
		lineStart--
	}
	prefix := bytes.TrimSpace(src[lineStart:start])
	return len(bytes.TrimSpace(bytes.TrimPrefix(prefix, bom))) == 0
}

func atColumnZero(src []byte, start int) bool {
	return start == 0 || src[start-1] == '\n'
}

func comments(src []byte) []comment {
	file := token.NewFileSet().AddFile("", 1, len(src))
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) {}, scanner.ScanComments)
	buildEnd := plusBuildEnd(src)
	var found []comment
	header := true
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			return found
		}
		switch tok {
		case token.PACKAGE:
			header = false
		case token.COMMENT:
			sp := span{start: file.Offset(pos)}
			sp.end = commentEnd(src, sp.start)
			found = append(found, comment{span: sp, name: directiveName(src, sp, header, buildEnd), load: loadBearing(src, sp)})
		}
	}
}

func scan(src []byte) []span {
	var spans []span
	for _, c := range comments(src) {
		if c.name == "" {
			spans = append(spans, c.span)
		}
	}
	return spans
}

func plusBuildEnd(src []byte) int {
	end := 0
	p := src
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), bom))
		switch {
		case len(trimmed) == 0:
			end = len(src) - len(p)
		case !bytes.HasPrefix(trimmed, []byte("//")):
			return end
		}
	}
	return end
}

func commentEnd(src []byte, start int) int {
	if start+1 < len(src) && src[start+1] == '/' {
		end := start
		for end < len(src) && src[end] != '\n' {
			end++
		}
		return end
	}
	if i := bytes.Index(src[start+2:], []byte("*/")); i >= 0 {
		return start + i + 4
	}
	return len(src)
}

func constraintSet(src []byte) string {
	var goBuild string
	var plus []string
	for _, c := range comments(src) {
		if c.name == "" {
			continue
		}
		line := string(src[c.span.start:c.span.end])
		switch c.name {
		case "go:build":
			if goBuild == "" {
				goBuild = line
			}
		case "+build":
			plus = append(plus, line)
		}
	}
	if goBuild != "" {
		return normalizedConstraint(goBuild)
	}
	var x constraint.Expr
	for _, line := range plus {
		y, err := constraint.Parse(line)
		if err != nil {
			x = nil
			break
		}
		if x == nil {
			x = y
		} else {
			x = &constraint.AndExpr{X: x, Y: y}
		}
	}
	if x == nil {
		return strings.Join(plus, " && ")
	}
	return x.String()
}

func normalizedConstraint(line string) string {
	x, err := constraint.Parse(line)
	if err != nil {
		return line
	}
	return x.String()
}

var printNode = format.Node

func strip(src []byte) ([]byte, error) {
	if lineDirective(src) {
		return nil, errors.New("refusing to write: the file maps positions with line directives")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	classified := map[int]bool{}
	for _, c := range comments(src) {
		if c.name != "" || c.load {
			classified[c.span.start] = true
		}
	}
	protected := generateLines(src)
	keep := map[*ast.CommentGroup]bool{}
	for _, g := range f.Comments {
		for _, c := range g.List {
			start := fset.Position(c.Pos()).Offset
			if classified[start] || toolchainMarker(c.Text) || protectedIn(protected, start, fset.Position(c.End()).Offset) {
				keep[g] = true
				break
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			return true
		}
		for _, spec := range gd.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok || imp.Path == nil || imp.Path.Value != `"C"` {
				continue
			}
			for _, g := range []*ast.CommentGroup{imp.Doc, imp.Comment, gd.Doc} {
				if g != nil {
					keep[g] = true
				}
			}
		}
		return true
	})
	drop := func(g *ast.CommentGroup) *ast.CommentGroup {
		if g == nil || keep[g] {
			return g
		}
		return nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.File:
			d.Doc = drop(d.Doc)
		case *ast.GenDecl:
			d.Doc = drop(d.Doc)
		case *ast.FuncDecl:
			d.Doc = drop(d.Doc)
		case *ast.Field:
			d.Doc, d.Comment = drop(d.Doc), drop(d.Comment)
		case *ast.ImportSpec:
			d.Doc, d.Comment = drop(d.Doc), drop(d.Comment)
		case *ast.ValueSpec:
			d.Doc, d.Comment = drop(d.Doc), drop(d.Comment)
		case *ast.TypeSpec:
			d.Doc, d.Comment = drop(d.Doc), drop(d.Comment)
		}
		return true
	})
	var kept []*ast.CommentGroup
	for _, g := range f.Comments {
		if keep[g] {
			kept = append(kept, g)
		}
	}
	if len(kept) == len(f.Comments) {
		return src, nil
	}
	f.Comments = kept
	var out bytes.Buffer
	if err := printNode(&out, fset, f); err != nil {
		return nil, err
	}
	res := out.Bytes()
	if !sameCommands(generateCommands(src), generateCommands(res)) {
		return nil, errors.New("refusing to write: a //go:generate directive would move or activate")
	}
	if goBuildCount(res) > 1 {
		return nil, errors.New("refusing to write: the reprint would leave multiple //go:build comments")
	}
	before, after := constraintExprs(src), constraintExprs(res)
	if !subsetExprs(after, before) || !keptConstraintsSurvive(kept, after) {
		return nil, errors.New("refusing to write: the reprint would rewrite the build constraint comments")
	}
	for _, g := range kept {
		for _, c := range g.List {
			if !strings.HasPrefix(c.Text, "//") || isConstraintLine(c.Text) {
				continue
			}
			if !bytes.Contains(res, []byte(c.Text)) {
				return nil, errors.New("refusing to write: a comment the keep rules preserve would not survive the reprint")
			}
		}
	}
	if lineDirective(res) {
		return nil, errors.New("refusing to write: the reprint would activate a line directive")
	}
	if before := cgoPreambles(fset, f); len(before) > 0 {
		afterFset := token.NewFileSet()
		after, err := parser.ParseFile(afterFset, "", res, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		if !samePreambles(before, cgoPreambles(afterFset, after)) {
			return nil, errors.New("refusing to write: the cgo preamble would change")
		}
	}
	if err := checkEquivalent(src, res); err != nil {
		return nil, err
	}
	return res, nil
}

type genCmd struct {
	line int
	text string
	eof  bool
}

func generateCommands(src []byte) []genCmd {
	var cmds []genCmd
	line := 1
	for off := 0; off <= len(src); {
		end := off
		for end < len(src) && src[end] != '\n' {
			end++
		}
		text := src[off:end]
		if len(text) > 0 && text[len(text)-1] == '\r' {
			text = text[:len(text)-1]
		}
		if bytes.HasPrefix(text, []byte("//go:generate ")) || bytes.HasPrefix(text, []byte("//go:generate\t")) {
			cmds = append(cmds, genCmd{line: line, text: string(text), eof: end >= len(src)})
		}
		if end >= len(src) {
			break
		}
		off = end + 1
		line++
	}
	return cmds
}

func containsCommand(cmds []genCmd, want genCmd) bool {
	for _, cmd := range cmds {
		if cmd == want {
			return true
		}
	}
	return false
}

func sameCommands(a, b []genCmd) bool {
	for _, cmd := range a {
		if !containsCommand(b, cmd) {
			return false
		}
	}
	for _, cmd := range b {
		if !containsCommand(a, cmd) {
			return false
		}
	}
	return true
}

func goBuildCount(src []byte) int {
	n := 0
	for _, c := range comments(src) {
		if constraint.IsGoBuild(string(src[c.span.start:c.span.end])) {
			n++
		}
	}
	return n
}

func isConstraintLine(text string) bool {
	return constraint.IsGoBuild(text) || constraint.IsPlusBuild(text)
}

func constraintExprs(src []byte) map[string]bool {
	set := map[string]bool{}
	for _, c := range comments(src) {
		text := string(src[c.span.start:c.span.end])
		if !isConstraintLine(text) {
			continue
		}
		x, err := constraint.Parse(text)
		if err != nil {
			set[text] = true
			continue
		}
		set[x.String()] = true
	}
	return set
}

func subsetExprs(sub, super map[string]bool) bool {
	for k := range sub {
		if !super[k] {
			return false
		}
	}
	return true
}

func keptConstraintsSurvive(kept []*ast.CommentGroup, after map[string]bool) bool {
	for _, g := range kept {
		for _, c := range g.List {
			if !isConstraintLine(c.Text) {
				continue
			}
			x, err := constraint.Parse(c.Text)
			if err != nil {
				if !after[c.Text] {
					return false
				}
				continue
			}
			if !after[x.String()] {
				return false
			}
		}
	}
	return true
}

func generateLines(src []byte) []int {
	var offs []int
	off := 0
	for off <= len(src) {
		end := off
		for end < len(src) && src[end] != '\n' {
			end++
		}
		line := src[off:end]
		if bytes.HasPrefix(line, []byte("//go:generate ")) || bytes.HasPrefix(line, []byte("//go:generate\t")) {
			offs = append(offs, off)
		}
		if end >= len(src) {
			break
		}
		off = end + 1
	}
	return offs
}

func protectedIn(offs []int, start, end int) bool {
	for _, o := range offs {
		if o >= start && o < end {
			return true
		}
	}
	return false
}

func toolchainMarker(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	body := text
	if strings.HasPrefix(body, "/*") {
		body = strings.TrimSuffix(strings.TrimPrefix(body, "/*"), "*/")
	} else if strings.HasPrefix(body, "//") {
		body = body[2:]
	}
	trimmed := strings.TrimSpace(body)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "output:") || strings.HasPrefix(lower, "unordered output:") {
		return true
	}
	if rest, ok := strings.CutPrefix(trimmed, "import"); ok {
		if rest == "" {
			return true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		if !(unicode.IsLetter(r) || '0' <= r && r <= '9' || r == '_') {
			return true
		}
	}
	return false
}

type cgoPreamble struct {
	line int
	text []string
}

func cgoPreambles(fset *token.FileSet, f *ast.File) []cgoPreamble {
	var groups []cgoPreamble
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			return true
		}
		for _, spec := range gd.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok || imp.Path == nil || imp.Path.Value != `"C"` {
				continue
			}
			for _, g := range []*ast.CommentGroup{imp.Doc, gd.Doc} {
				if g == nil {
					continue
				}
				p := cgoPreamble{line: fset.Position(g.Pos()).Line}
				for _, c := range g.List {
					p.text = append(p.text, c.Text)
				}
				groups = append(groups, p)
			}
		}
		return true
	})
	return groups
}

func samePreambles(a, b []cgoPreamble) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].line != b[i].line || len(a[i].text) != len(b[i].text) {
			return false
		}
		for j := range a[i].text {
			if a[i].text[j] != b[i].text[j] {
				return false
			}
		}
	}
	return true
}

func checkEquivalent(before, after []byte) error {
	same, err := sameAST(before, after)
	if err != nil {
		return err
	}
	if !same {
		return errors.New("refusing to write: stripping would change the program")
	}
	if constraintSet(before) != constraintSet(after) {
		return errors.New("refusing to write: stripping would change the build constraints")
	}
	x, err := exampleSet(before)
	if err != nil {
		return err
	}
	y, err := exampleSet(after)
	if err != nil {
		return err
	}
	if x != y {
		return errors.New("refusing to write: stripping would change example execution")
	}
	return nil
}

func exampleSet(src []byte) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ex := range doc.Examples(f) {
		fmt.Fprintf(&b, "%s %q %t %t\n", ex.Name, ex.Output, ex.Unordered, ex.EmptyOutput)
	}
	return b.String(), nil
}

var posType = reflect.TypeOf(token.Pos(0))

func sameAST(before, after []byte) (bool, error) {
	first, err := astDump(before)
	if err != nil {
		return false, err
	}
	second, err := astDump(after)
	if err != nil {
		return false, err
	}
	return first == second, nil
}

func astDump(src []byte) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := ast.Fprint(&b, token.NewFileSet(), f, skipPositions); err != nil {
		return "", err
	}
	return b.String(), nil
}

func skipPositions(name string, value reflect.Value) bool {
	return value.Type() != posType
}
