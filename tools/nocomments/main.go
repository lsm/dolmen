package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/version"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const allowlistPath = "scripts/no-comments-allowlist.txt"

type span struct{ start, end int }

type scan struct{ comments []span }

const wsClass = `[\t\n\v\f\r\x85\p{Zs}\x{2028}\x{2029}]`

var (
	buildTagPattern  = regexp.MustCompile(`^//go:build(` + wsClass + `.*)?$`)
	legacyBuildLine  = regexp.MustCompile(`^//` + wsClass + `*\+build(` + wsClass + `.*)?$`)
	linePattern      = regexp.MustCompile(`^//line .*:[0-9]*[1-9][0-9]*(?::[0-9]*[1-9][0-9]*)?\r?$`)
	blockLinePattern = regexp.MustCompile(`(?s)^/\*line .*:[0-9]*[1-9][0-9]*(?::[0-9]*[1-9][0-9]*)?\*/$`)
	headerBlankLine  = regexp.MustCompile(`\n[ \t\r]*\n`)
	exportPattern    = regexp.MustCompile(`^//export .+\r?$`)
	nolintPattern    = regexp.MustCompile(`^//nolint(:[0-9A-Za-z_]+(-[0-9A-Za-z_]+)*(,[0-9A-Za-z_]+(-[0-9A-Za-z_]+)*)*)?([ \t].*)?\r?$`)
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
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "C" && imp.Name == nil {
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

func embedQualifiers(f *ast.File) (map[string]bool, bool) {
	qualifiers := map[string]bool{}
	dot := false
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
			if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != "embed" {
				continue
			}
			name := "embed"
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == "." {
				dot = true
			} else if name != "_" {
				qualifiers[name] = true
			}
		}
	}
	return qualifiers, dot
}

type embedKind int

const (
	embedUnknown embedKind = iota
	embedBytes
	embedString
	embedFiles
	embedNamed
)

var scalarBuiltins = map[string]bool{
	"bool": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "float32": true, "float64": true, "complex64": true, "complex128": true,
	"byte": true, "rune": true, "error": true, "any": true, "comparable": true,
}

func embeddableType(expr ast.Expr, qualifiers map[string]bool, dot bool) embedKind {
	switch t := expr.(type) {
	case *ast.ParenExpr:
		return embeddableType(t.X, qualifiers, dot)
	case *ast.Ident:
		if t.Name == "string" {
			return embedString
		}
		if dot && t.Name == "FS" {
			return embedFiles
		}
		if !scalarBuiltins[t.Name] {
			return embedNamed
		}
	case *ast.ArrayType:
		if elt, ok := t.Elt.(*ast.Ident); ok && t.Len == nil {
			if elt.Name == "byte" || elt.Name == "uint8" {
				return embedBytes
			}
			if !scalarBuiltins[elt.Name] {
				return embedNamed
			}
		}
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && qualifiers[pkg.Name] && t.Sel.Name == "FS" {
			return embedFiles
		}
		return embedNamed
	}
	return embedUnknown
}

var goDirectives = map[string]bool{
	"generate": true, "embed": true, "linkname": true, "debug": true,
	"noinline": true, "nosplit": true, "norace": true, "nocheckptr": true,
	"uintptrescapes": true, "registerparams": true, "nointerface": true,
	"noescape": true, "wasmimport": true, "wasmexport": true,
}

func goDirective(norm []byte) (string, []string, bool) {
	d, ok := ast.ParseDirective(0, string(norm))
	if !ok || d.Tool != "go" || !goDirectives[d.Name] {
		return "", nil, false
	}
	if end := 2 + len(d.Tool) + 1 + len(d.Name); end < len(norm) && norm[end] != ' ' && norm[end] != '\t' {
		return "", nil, false
	}
	parsed, err := d.ParseArgs()
	if err != nil {
		return "", nil, false
	}
	args := make([]string, 0, len(parsed))
	for _, a := range parsed {
		args = append(args, a.Arg)
	}
	return d.Name, args, true
}

func validEmbedPatterns(patterns []string) bool {
	for _, p := range patterns {
		g, _ := strings.CutPrefix(p, "all:")
		if _, err := path.Match(g, ""); err != nil {
			return false
		}
		for _, part := range strings.Split(g, "/") {
			if part == "" || part == "." || part == ".." {
				return false
			}
		}
	}
	return true
}

func embedMultiFile(kind embedKind, patterns []string) bool {
	if kind != embedString && kind != embedBytes {
		return false
	}
	uniq := map[string]bool{}
	for _, p := range patterns {
		g, _ := strings.CutPrefix(p, "all:")
		if strings.ContainsAny(g, "*?[\\") {
			return false
		}
		uniq[g] = true
	}
	return len(uniq) > 1
}

func embeddableSpec(sp *ast.ValueSpec, qualifiers map[string]bool, dot bool) embedKind {
	if len(sp.Names) != 1 {
		return embedUnknown
	}
	return embeddableType(sp.Type, qualifiers, dot)
}

func docGroups(f *ast.File) (map[token.Pos]embedKind, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]string) {
	valueDocs := map[token.Pos]embedKind{}
	funcDocs := map[token.Pos]bool{}
	bodylessDocs := map[token.Pos]bool{}
	bodiedDocs := map[token.Pos]bool{}
	bareFuncDocs := map[token.Pos]bool{}
	funcNames := map[token.Pos]string{}
	mark := func(m map[token.Pos]bool, cg *ast.CommentGroup) {
		if cg != nil {
			m[cg.Pos()] = true
		}
	}
	qualifiers, dot := embedQualifiers(f)
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			if decl.Tok == token.VAR {
				allEmbeddable := true
				declKind := embedUnknown
				for _, s := range decl.Specs {
					sp, ok := s.(*ast.ValueSpec)
					if !ok || len(sp.Values) > 0 {
						allEmbeddable = false
						continue
					}
					k := embeddableSpec(sp, qualifiers, dot)
					if k == embedUnknown {
						allEmbeddable = false
						continue
					}
					if sp.Doc != nil {
						valueDocs[sp.Doc.Pos()] = k
					}
					if declKind == embedUnknown {
						declKind = k
					} else if declKind != k {
						declKind = embedUnknown
						allEmbeddable = false
					}
				}
				if allEmbeddable && declKind != embedUnknown && decl.Doc != nil && !decl.Lparen.IsValid() {
					valueDocs[decl.Doc.Pos()] = declKind
				}
			}
		case *ast.FuncDecl:
			mark(funcDocs, decl.Doc)
			if decl.Doc != nil {
				funcNames[decl.Doc.Pos()] = decl.Name.Name
			}
			if decl.Recv != nil {
				break
			}
			if decl.Doc != nil {
				bareFuncDocs[decl.Doc.Pos()] = true
			}
			if decl.Body == nil {
				mark(bodylessDocs, decl.Doc)
			} else {
				mark(bodiedDocs, decl.Doc)
			}
		}
	}
	return valueDocs, funcDocs, bodylessDocs, bodiedDocs, bareFuncDocs, funcNames
}

func atLineStart(src []byte, start int) bool {
	return start == 0 || src[start-1] == '\n'
}

func blank(b []byte) bool { return len(bytes.Trim(b, " \t\r")) == 0 }

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func fileImports(f *ast.File, want string) bool {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gd.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if ok {
				if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == want {
					return true
				}
			}
		}
	}
	return false
}

func fileImportsUnrenamed(f *ast.File, want string) bool {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gd.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if ok && imp.Name == nil {
				if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == want {
					return true
				}
			}
		}
	}
	return false
}

var godebugKeys = map[string]bool{}

func init() {
	for _, k := range []string{
		"allowmultiplevcs", "asynctimerchan", "containermaxprocs", "cryptocustomrand", "dataindependenttiming",
		"decoratemappings", "embedfollowsymlinks", "execerrdot", "fips140", "fips140ems", "gocachehash",
		"gocachetest", "gocacheverify", "gotestjsonbuildtext", "gotypesalias", "htmlmetacontenturlescape",
		"http2client", "http2debug", "http2server", "httpcookiemaxnum", "httplaxcontentlength", "httpmuxgo121",
		"httpservecontentkeepheaders", "installgoroot", "jstmpllitinterp", "multipartfiles", "multipartmaxheaders",
		"multipartmaxparts", "multipathtcp", "netdns", "netedns0", "panicnil", "randautoseed", "randseednop",
		"rsa1024min", "tarinsecurepath", "tls10server", "tls3des", "tlsmaxrsasize", "tlsmlkem", "tlsrsakex",
		"tlssecpmlkem", "tlssha1", "tlsunsafeekm", "updatemaxprocs", "urlmaxqueryparams", "urlstrictcolons",
		"winreadlinkvolume", "winsymlink", "x509keypairleaf", "x509negativeserial", "x509rsacrt", "x509sha1",
		"x509sha256skid", "x509usefallbackroots", "x509usepolicies", "zipinsecurepath",
	} {
		godebugKeys[k] = true
	}
}

var godebugDefaultPattern = regexp.MustCompile(`^go[1-9][0-9]*(\.[0-9]+)*([a-z]+[0-9]*)?$`)

func validDebugArgs(args []string) bool {
	if len(args) != 1 {
		return false
	}
	k, v, ok := strings.Cut(args[0], "=")
	if !ok || strings.ContainsAny(v, ", \t") {
		return false
	}
	if k == "default" {
		return godebugDefaultPattern.MatchString(v) && !versionTooNew(v)
	}
	return godebugKeys[k]
}

func versionTooNew(v string) bool {
	cur := runtime.Version()
	if !version.IsValid(v) || !version.IsValid(cur) {
		return false
	}
	return version.Compare(v, cur) > 0
}

func validBuildConstraint(raw []byte) bool {
	line := strings.TrimSpace(string(raw))
	if !buildTagPattern.Match(raw) {
		return false
	}
	_, err := constraint.Parse(line)
	return err == nil
}

type exempts struct {
	atLineStart bool
	directive   string
	args        []string
	valueDoc    bool
	valueKind   embedKind
	embedArgs   []string
	funcDoc     bool
	bareFunc    bool
	bodylessDoc bool
	bodiedDoc   bool
	unsafe      bool
	embed       bool
	mainish     bool
	header      bool
	lineOnly    bool
}

func isExempt(raw, norm []byte, c exempts) bool {
	if c.atLineStart && linePattern.Match(raw) {
		return true
	}
	switch c.directive {
	case "generate":
		if c.atLineStart && len(c.args) > 0 {
			return true
		}
	case "embed":
		if c.valueDoc && c.embed && len(c.args) > 0 && validEmbedPatterns(c.args) && !embedMultiFile(c.valueKind, c.embedArgs) {
			return true
		}
	case "linkname":
		if c.unsafe && c.lineOnly && len(c.args) >= 1 && len(c.args) <= 2 {
			return true
		}
	case "debug":
		if c.mainish && c.header && validDebugArgs(c.args) {
			return true
		}
	case "noinline", "nosplit", "norace", "nocheckptr", "uintptrescapes", "registerparams", "nointerface":
		if c.funcDoc && len(c.args) == 0 {
			return true
		}
	case "noescape":
		if c.bodylessDoc && len(c.args) == 0 {
			return true
		}
	case "wasmimport":
		if c.bodylessDoc && len(c.args) == 2 {
			return true
		}
	case "wasmexport":
		if c.bareFunc && c.bodiedDoc && len(c.args) == 1 {
			return true
		}
	}
	return blockLinePattern.Match(raw) || nolintPattern.Match(norm)
}

func isGeneratedMarker(text []byte) bool {
	text = bytes.ReplaceAll(text, []byte("\r"), nil)
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
	pkgOff := tf.Offset(f.Package)
	var headerBlockStarts []int
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			start := tf.Offset(c.Pos())
			if !bytes.HasPrefix(src[start:], []byte("//")) {
				headerBlockStarts = append(headerBlockStarts, start)
			}
		}
	}
	preambles := cgoPreambles(f)
	var outputs map[token.Pos]bool
	if strings.HasSuffix(path, "_test.go") {
		outputs = exampleOutputs(f)
	}
	valueDocs, funcDocs, bodylessDocs, bodiedDocs, bareFuncDocs, funcNames := docGroups(f)
	isCgo := fileImportsUnrenamed(f, "C")
	hasUnsafe := fileImports(f, "unsafe")
	hasEmbed := fileImports(f, "embed")
	isMainish := f.Name.Name == "main" || strings.HasSuffix(path, "_test.go")
	s := &scan{}
	for _, g := range f.Comments {
		if preambles[g.Pos()] || outputs[g.Pos()] {
			continue
		}
		kind := valueDocs[g.Pos()]
		var embedArgs []string
		if kind != embedUnknown && hasEmbed {
			for _, c := range g.List {
				start := tf.Offset(c.Pos())
				norm := bytes.ReplaceAll(src[start:commentEnd(src, start)], []byte("\r"), nil)
				if name, args, ok := goDirective(norm); ok && name == "embed" {
					embedArgs = append(embedArgs, args...)
				}
			}
		}
		for _, c := range g.List {
			start := tf.Offset(c.Pos())
			end := commentEnd(src, start)
			raw := src[start:end]
			norm := bytes.ReplaceAll(raw, []byte("\r"), nil)
			if c.Pos() < f.Package && isGeneratedMarker(norm) {
				continue
			}
			lineLead := src[bytes.LastIndexByte(src[:start], '\n')+1 : start]
			if bytes.HasPrefix(lineLead, utf8BOM) {
				lineLead = lineLead[len(utf8BOM):]
			}
			if c.Pos() < f.Package && blank(lineLead) && validBuildConstraint(raw) {
				continue
			}
			blockBefore := false
			searchEnd := pkgOff
			for _, b := range headerBlockStarts {
				if b < start {
					blockBefore = true
				} else if b < searchEnd {
					searchEnd = b
				}
			}
			if c.Pos() < f.Package && !blockBefore &&
				legacyBuildLine.Match(raw) && headerBlankLine.Match(src[end:searchEnd]) {
				continue
			}
			if bareFuncDocs[g.Pos()] && isCgo && exportPattern.Match(norm) {
				if name := strings.TrimSpace(string(bytes.TrimPrefix(norm, []byte("//export ")))); name == funcNames[g.Pos()] {
					continue
				}
			}
			name, args, isDirective := goDirective(norm)
			if !isDirective {
				name, args = "", nil
			}
			if isExempt(raw, norm, exempts{
				atLineStart: atLineStart(src, start),
				directive:   name,
				args:        args,
				valueDoc:    kind != embedUnknown,
				valueKind:   kind,
				embedArgs:   embedArgs,
				funcDoc:     funcDocs[g.Pos()],
				bareFunc:    bareFuncDocs[g.Pos()],
				bodylessDoc: bodylessDocs[g.Pos()],
				bodiedDoc:   bodiedDocs[g.Pos()],
				unsafe:      hasUnsafe,
				embed:       hasEmbed,
				mainish:     isMainish,
				header:      c.Pos() < f.Package,
				lineOnly:    blank(lineLead),
			}) {
				continue
			}
			s.comments = append(s.comments, span{start, end})
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

func updateBlockState(line []byte, inBlock bool) bool {
	i := 0
	for i < len(line) {
		if inBlock {
			k := bytes.Index(line[i:], []byte("*/"))
			if k < 0 {
				return true
			}
			i += k + 2
			inBlock = false
			continue
		}
		if bytes.HasPrefix(line[i:], []byte("//")) {
			return false
		}
		if bytes.HasPrefix(line[i:], []byte("/*")) {
			inBlock = true
			i += 2
			continue
		}
		i++
	}
	return inBlock
}

func forEachHeaderLine(src []byte, fn func(trimmed []byte)) {
	src = bytes.TrimPrefix(src, utf8BOM)
	inBlock := false
	for _, line := range bytes.Split(src, []byte("\n")) {
		if !inBlock {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("package ")) || bytes.Equal(trimmed, []byte("package")) {
				return
			}
			fn(trimmed)
		}
		inBlock = updateBlockState(line, inBlock)
	}
}

func headerHasBuildConstraint(src []byte) bool {
	found := false
	forEachHeaderLine(src, func(trimmed []byte) {
		if bytes.HasPrefix(trimmed, []byte("//go:build")) {
			found = true
			return
		}
		comment := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("//")))
		if bytes.HasPrefix(comment, []byte("+build")) {
			found = true
		}
	})
	return found
}

func normalizeHeaderSpace(src []byte) []byte {
	limit := headerLimit(src)
	var out []byte
	start := 0
	if bytes.HasPrefix(src, utf8BOM) {
		start = len(utf8BOM)
		out = append(out, utf8BOM...)
	}
	for p := start; p < limit; {
		nl := bytes.IndexByte(src[p:limit], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = limit
		} else {
			lineEnd = p + nl
		}
		line := src[p:lineEnd]
		k, spaces := 0, 0
		for k < len(line) {
			r, size := utf8.DecodeRune(line[k:])
			if r != '\n' && unicode.IsSpace(r) {
				k += size
				spaces++
				continue
			}
			break
		}
		out = append(out, bytes.Repeat([]byte(" "), spaces)...)
		out = append(out, line[k:]...)
		if lineEnd < limit {
			out = append(out, '\n')
		}
		if nl < 0 {
			break
		}
		p = lineEnd + 1
	}
	return append(out, src[limit:]...)
}

func headerLimit(src []byte) int {
	bom := 0
	if bytes.HasPrefix(src, utf8BOM) {
		bom = len(utf8BOM)
	}
	limit := len(src)
	p := bom
	inBlock := false
	for _, line := range bytes.Split(src[bom:], []byte("\n")) {
		lineStart := p
		if !inBlock {
			if trimmed := bytes.TrimSpace(line); bytes.HasPrefix(trimmed, []byte("package ")) || bytes.Equal(trimmed, []byte("package")) {
				return lineStart
			}
		}
		inBlock = updateBlockState(line, inBlock)
		p += len(line) + 1
	}
	return limit
}

func lexCount(src []byte) int {
	count := 0
	limit := headerLimit(src)
	var headerBlocks []int
	for k := 0; k+1 < limit; k++ {
		if src[k] == '/' && src[k+1] == '*' {
			headerBlocks = append(headerBlocks, k)
		}
	}
	i, n := 0, len(src)
	for i < n {
		switch src[i] {
		case '"', '\'':
			q, j := src[i], i+1
			for j < n && src[j] != q && src[j] != '\n' {
				if src[j] == '\\' && j+1 < n && src[j+1] != '\n' {
					j++
				}
				j++
			}
			i = j + 1
		case '`':
			k := bytes.IndexByte(src[i+1:], '`')
			if k < 0 {
				i = n
			} else {
				i += k + 2
			}
		case '/':
			if i+1 < n && src[i+1] == '/' {
				j := i + 2
				for j < n && src[j] != '\n' {
					j++
				}
				raw := src[i:j]
				norm := bytes.ReplaceAll(raw, []byte("\r"), nil)
				name, args, isDirective := goDirective(norm)
				if !isDirective {
					name, args = "", nil
				}
				exempt := isExempt(raw, norm, exempts{atLineStart: atLineStart(src, i), directive: name, args: args})
				if i < limit {
					lead := src[bytes.LastIndexByte(src[:i], '\n')+1 : i]
					lead = bytes.TrimPrefix(lead, utf8BOM)
					if len(bytes.TrimFunc(lead, unicode.IsSpace)) == 0 && validBuildConstraint(raw) {
						exempt = true
					}
					if legacyBuildLine.Match(raw) {
						blockBefore, searchEnd := false, limit
						for _, b := range headerBlocks {
							if b < i {
								blockBefore = true
							} else if b < searchEnd {
								searchEnd = b
							}
						}
						if !blockBefore && headerBlankLine.Match(src[j:searchEnd]) {
							exempt = true
						}
					}
				}
				if !exempt {
					count++
				}
				i = j
			} else if i+1 < n && src[i+1] == '*' {
				k := bytes.Index(src[i+2:], []byte("*/"))
				if k < 0 {
					if !isExempt(src[i:], bytes.ReplaceAll(src[i:], []byte("\r"), nil), exempts{}) {
						count++
					}
					i = n
				} else {
					span := src[i : i+k+4]
					if !isExempt(span, bytes.ReplaceAll(span, []byte("\r"), nil), exempts{}) {
						count++
					}
					i += k + 4
				}
			} else {
				i++
			}
		default:
			i++
		}
	}
	return count
}

func scanFile(src []byte, path string) (bool, *scan, error) {
	s, err := scanSource(src, path)
	if err != nil && headerHasBuildConstraint(src) {
		if s2, err2 := scanSource(normalizeHeaderSpace(src), path); err2 == nil {
			return false, s2, nil
		}
		return false, &scan{comments: make([]span, lexCount(src))}, nil
	}
	if err != nil {
		return false, nil, err
	}
	return false, s, nil
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
	srcs := map[string][]byte{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			die(err)
		}
		srcs[f] = src
	}

	allow := map[string]int{}
	if mode == "check" {
		if allow, err = readAllowlist(allowlistPath); err != nil {
			die(err)
		}
	}
	bad := 0
	for _, f := range files {
		skip, s, err := scanFile(srcs[f], f)
		if err != nil {
			die(fmt.Errorf("cannot parse %s: %w", f, err))
		}
		if skip {
			continue
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
