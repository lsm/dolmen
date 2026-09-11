package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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

const wsClass = `[\t\n\v\f\r\x85\p{Zs}\x{2028}\x{2029}]`

var (
	funcPragmaPattern     = regexp.MustCompile(`^//go:(?:noinline|nosplit|norace|nocheckptr|uintptrescapes|registerparams|nointerface)$`)
	bodylessPragmaPattern = regexp.MustCompile(`^//go:(?:noescape$|wasmimport[ \t]+\S+[ \t]+\S+[ \t]*$)`)
	wasmexportPattern     = regexp.MustCompile(`^//go:wasmexport[ \t]+\S+[ \t]*$`)
	embedPattern          = regexp.MustCompile(`^//go:embed [^\r\n]+$`)
	generatePattern       = regexp.MustCompile(`^//go:generate[ \t].+\r?$`)
	bareGenerate          = regexp.MustCompile(`^//go:generate[ \t]*\r?$`)
	buildTagPattern       = regexp.MustCompile(`^//go:build(` + wsClass + `.*)?$`)
	legacyBuildLine       = regexp.MustCompile(`^//` + wsClass + `*\+build(` + wsClass + `.*)?$`)
	linePattern           = regexp.MustCompile(`^//line .*:[1-9][0-9]*(?::[1-9][0-9]*)?\r?$`)
	blockLinePattern      = regexp.MustCompile(`(?s)^/\*line .*:[1-9][0-9]*(?::[1-9][0-9]*)?\*/$`)
	debugPattern          = regexp.MustCompile(`^//go:debug[ \t]+[A-Za-z0-9_.-]+=[^ \t\r\n,]*$`)
	headerBlankLine       = regexp.MustCompile(`\n[ \t\r]*\n`)
	exportPattern         = regexp.MustCompile(`^//export .+\r?$`)
	nolintPattern         = regexp.MustCompile(`^//nolint(:[0-9A-Za-z_]+(-[0-9A-Za-z_]+)*(,[0-9A-Za-z_]+(-[0-9A-Za-z_]+)*)*)?([ \t].*)?\r?$`)
	linknamePattern       = regexp.MustCompile(`^//go:linkname[ \t]+\S+(?:[ \t]+\S+)?[ \t]*$`)
	outputPattern         = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)
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

func embeddableType(expr ast.Expr, qualifiers map[string]bool, dot bool) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "string" || (dot && t.Name == "FS")
	case *ast.ArrayType:
		if elt, ok := t.Elt.(*ast.Ident); ok {
			return t.Len == nil && elt.Name == "byte"
		}
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return qualifiers[pkg.Name] && t.Sel.Name == "FS"
		}
	}
	return false
}

func validEmbedPatterns(norm []byte) bool {
	fields := strings.Fields(string(norm[len("//go:embed"):]))
	if len(fields) == 0 {
		return false
	}
	for _, p := range fields {
		if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
			return false
		}
		for _, part := range strings.Split(p, "/") {
			if part == "" || part == "." || part == ".." {
				return false
			}
		}
	}
	return true
}

func embeddableSpec(sp *ast.ValueSpec, qualifiers map[string]bool, dot bool) bool {
	return len(sp.Names) == 1 && embeddableType(sp.Type, qualifiers, dot)
}

func docGroups(f *ast.File) (map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]bool, map[token.Pos]string) {
	valueDocs := map[token.Pos]bool{}
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
				for _, s := range decl.Specs {
					sp, ok := s.(*ast.ValueSpec)
					if !ok || len(sp.Values) > 0 || !embeddableSpec(sp, qualifiers, dot) {
						allEmbeddable = false
						continue
					}
					mark(valueDocs, sp.Doc)
				}
				if allEmbeddable {
					mark(valueDocs, decl.Doc)
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

var knownOS = map[string]bool{"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true}
var knownArch = map[string]bool{"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true, "arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true}

func fileTags(path string, f *ast.File) []string {
	tags := map[string]bool{}
	base := filepath.Base(path)
	if i := strings.Index(base, "_"); i >= 0 {
		l := strings.Split(base[i:], "_")
		if n := len(l); n > 0 && l[n-1] == "test" {
			l = l[:n-1]
		}
		n := len(l)
		if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
			tags[l[n-2]] = true
			tags[l[n-1]] = true
		} else if n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
			tags[l[n-1]] = true
		}
	}
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			if x, err := constraint.Parse(strings.TrimSpace(c.Text)); err == nil {
				if b, ok := x.(*constraint.TagExpr); ok {
					tags[b.Tag] = true
				}
			}
		}
	}
	out := make([]string, 0, len(tags))
	for t := range tags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
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

func validDebugSettings(norm []byte) bool {
	if !debugPattern.Match(norm) {
		return false
	}
	setting := strings.TrimSpace(string(norm[len("//go:debug"):]))
	eq := strings.IndexByte(setting, '=')
	k, v := setting[:eq], setting[eq+1:]
	if k == "default" {
		return godebugDefaultPattern.MatchString(v)
	}
	return godebugKeys[k]
}

func validBuildConstraint(raw []byte) bool {
	line := strings.TrimSpace(string(raw))
	if !buildTagPattern.Match(raw) {
		return false
	}
	_, err := constraint.Parse(line)
	return err == nil
}

func linknameNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.VAR {
				continue
			}
			for _, s := range decl.Specs {
				if sp, ok := s.(*ast.ValueSpec); ok {
					for _, n := range sp.Names {
						if n.Name != "_" {
							names[n.Name] = true
						}
					}
				}
			}
		case *ast.FuncDecl:
			if decl.Recv == nil {
				if decl.Name.Name != "_" {
					names[decl.Name.Name] = true
				}
			}
		}
	}
	return names
}

type exempts struct {
	atLineStart bool
	valueDoc    bool
	funcDoc     bool
	bodylessDoc bool
	cgo         bool
	unsafe      bool
	embed       bool
	linkname    bool
}

func isExempt(raw, norm []byte, c exempts) bool {
	if c.atLineStart && linePattern.Match(raw) {
		return true
	}
	if c.atLineStart && generatePattern.Match(raw) && !bareGenerate.Match(raw) {
		return true
	}
	if c.funcDoc && funcPragmaPattern.Match(norm) {
		return true
	}
	if c.bodylessDoc && bodylessPragmaPattern.Match(norm) {
		return true
	}
	if c.valueDoc && c.embed && embedPattern.Match(norm) && validEmbedPatterns(norm) {
		return true
	}
	return blockLinePattern.Match(raw) || nolintPattern.Match(norm) || (c.unsafe && c.linkname && linknamePattern.Match(norm))
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

func scanSource(src []byte, path string, names map[string]bool) (*scan, error) {
	fset, f, err := parseGo(src)
	if err != nil {
		return nil, fmt.Errorf("unparseable: %w", err)
	}
	if names == nil {
		names = linknameNames(f)
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
			if bareFuncDocs[g.Pos()] && bodiedDocs[g.Pos()] && wasmexportPattern.Match(norm) {
				continue
			}
			if c.Pos() < f.Package && isMainish && validDebugSettings(norm) {
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
			linknameOK := false
			if hasUnsafe && linknamePattern.Match(norm) {
				if local := strings.Fields(string(bytes.TrimPrefix(norm, []byte("//go:linkname")))); len(local) > 0 && names[local[0]] {
					linknameOK = true
				}
			}
			if isExempt(raw, norm, exempts{
				atLineStart: atLineStart(src, start),
				valueDoc:    valueDocs[g.Pos()],
				funcDoc:     funcDocs[g.Pos()],
				bodylessDoc: bodylessDocs[g.Pos()],
				cgo:         isCgo,
				unsafe:      hasUnsafe,
				embed:       hasEmbed,
				linkname:    linknameOK,
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
				exempt := isExempt(raw, norm, exempts{atLineStart: atLineStart(src, i)})
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

func scanFile(src []byte, path string, names map[string]bool) (bool, *scan, error) {
	s, err := scanSource(src, path, names)
	if err != nil && headerHasBuildConstraint(src) {
		if s2, err2 := scanSource(normalizeHeaderSpace(src), path, names); err2 == nil {
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
	type pkgKey struct{ dir, pkg, tag string }
	namesBy := map[pkgKey]map[string]bool{}
	tagOf := map[string]string{}
	srcs := map[string][]byte{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			die(err)
		}
		srcs[f] = src
		_, ast, err := parseGo(src)
		if err != nil {
			continue
		}
		key := pkgKey{filepath.Dir(f), ast.Name.Name, strings.Join(fileTags(f, ast), ",")}
		if namesBy[key] == nil {
			namesBy[key] = map[string]bool{}
		}
		for n := range linknameNames(ast) {
			namesBy[key][n] = true
		}
		tagOf[f] = key.tag
	}
	pkgOf := map[string]string{}
	pkgOfNames := map[string]map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f)
		pkg := ""
		if _, ast, err := parseGo(srcs[f]); err == nil {
			pkg = ast.Name.Name
		}
		merged := map[string]bool{}
		for _, tag := range []string{tagOf[f], ""} {
			for n := range namesBy[pkgKey{dir, pkg, tag}] {
				merged[n] = true
			}
		}
		key := dir + "\x00" + pkg + "\x00" + tagOf[f]
		if _, seen := pkgOfNames[key]; !seen {
			pkgOfNames[key] = merged
		}
		pkgOf[f] = key
	}

	allow := map[string]int{}
	if mode == "check" {
		if allow, err = readAllowlist(allowlistPath); err != nil {
			die(err)
		}
	}
	bad := 0
	for _, f := range files {
		skip, s, err := scanFile(srcs[f], f, pkgOfNames[pkgOf[f]])
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
