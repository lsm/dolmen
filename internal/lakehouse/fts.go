package lakehouse

import (
	"golang.org/x/text/unicode/norm"
	"math"
	"strings"
	"unicode"
)

type ftsToken2 struct {
	raw  string
	stem string
}

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "but": true, "by": true,
	"for": true, "if": true, "in": true, "into": true, "is": true, "it": true, "no": true, "not": true, "of": true,
	"on": true, "or": true, "such": true, "that": true, "the": true, "their": true, "then": true, "there": true,
	"these": true, "they": true, "this": true, "to": true, "was": true, "will": true, "with": true,
}

func analyze(text string) []ftsToken2 {
	var out []ftsToken2
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		raw := cur.String()
		cur.Reset()
		out = append(out, ftsToken2{raw: raw, stem: stem(raw)})
	}
	for _, r := range foldAccents(strings.ToLower(text)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func foldAccents(s string) string {
	for _, r := range s {
		if r > unicode.MaxASCII {
			var b strings.Builder
			for _, r := range norm.NFD.String(s) {
				if !unicode.Is(unicode.Mn, r) {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
	}
	return s
}

func Tokens(text string) []string {
	out := []string{}
	for _, t := range analyze(text) {
		if !stopWords[t.raw] {
			out = append(out, t.stem)
		}
	}
	return out
}

func isConsonant(w []rune, i int) bool {
	switch w[i] {
	case 'a', 'e', 'i', 'o', 'u':
		return false
	case 'y':
		return i == 0 || !isConsonant(w, i-1)
	}
	return true
}

func measure(w []rune) int {
	n, i := 0, 0
	for i < len(w) && isConsonant(w, i) {
		i++
	}
	for i < len(w) {
		for i < len(w) && !isConsonant(w, i) {
			i++
		}
		if i >= len(w) {
			break
		}
		n++
		for i < len(w) && isConsonant(w, i) {
			i++
		}
	}
	return n
}

func hasVowel(w []rune) bool {
	for i := range w {
		if !isConsonant(w, i) {
			return true
		}
	}
	return false
}

func doubleConsonant(w []rune) bool {
	n := len(w)
	return n >= 2 && w[n-1] == w[n-2] && isConsonant(w, n-1)
}

func cvc(w []rune) bool {
	n := len(w)
	if n < 3 || !isConsonant(w, n-3) || isConsonant(w, n-2) || !isConsonant(w, n-1) {
		return false
	}
	switch w[n-1] {
	case 'w', 'x', 'y':
		return false
	}
	return true
}

func hasSuffix(w []rune, s string) bool { return strings.HasSuffix(string(w), s) }

func trimSuffix(w []rune, s string) []rune { return w[:len(w)-len([]rune(s))] }

func replaceIf(w []rune, suffix, repl string, minMeasure int) ([]rune, bool) {
	if !hasSuffix(w, suffix) {
		return w, false
	}
	stemPart := trimSuffix(w, suffix)
	if measure(stemPart) > minMeasure {
		return append(stemPart, []rune(repl)...), true
	}
	return w, true
}

func stem(word string) string {
	w := []rune(word)
	if len(w) <= 2 {
		return word
	}
	for _, r := range w {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return word
		}
	}
	switch {
	case hasSuffix(w, "sses"):
		w = trimSuffix(w, "es")
	case hasSuffix(w, "ies"):
		w = trimSuffix(w, "es")
	case hasSuffix(w, "ss"):
	case hasSuffix(w, "s"):
		w = trimSuffix(w, "s")
	}
	step1b := false
	if hasSuffix(w, "eed") {
		if measure(trimSuffix(w, "eed")) > 0 {
			w = trimSuffix(w, "d")
		}
	} else if hasSuffix(w, "ed") && hasVowel(trimSuffix(w, "ed")) {
		w = trimSuffix(w, "ed")
		step1b = true
	} else if hasSuffix(w, "ing") && hasVowel(trimSuffix(w, "ing")) {
		w = trimSuffix(w, "ing")
		step1b = true
	}
	if step1b {
		switch {
		case hasSuffix(w, "at"), hasSuffix(w, "bl"), hasSuffix(w, "iz"):
			w = append(w, 'e')
		case doubleConsonant(w) && !hasSuffix(w, "l") && !hasSuffix(w, "s") && !hasSuffix(w, "z"):
			w = w[:len(w)-1]
		case measure(w) == 1 && cvc(w):
			w = append(w, 'e')
		}
	}
	if hasSuffix(w, "y") && hasVowel(trimSuffix(w, "y")) {
		w[len(w)-1] = 'i'
	}
	for _, p := range [][2]string{{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"}, {"izer", "ize"}, {"abli", "able"}, {"alli", "al"}, {"entli", "ent"}, {"eli", "e"}, {"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"}, {"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"}, {"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"}} {
		var done bool
		if w, done = replaceIf(w, p[0], p[1], 0); done {
			break
		}
	}
	for _, p := range [][2]string{{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""}} {
		var done bool
		if w, done = replaceIf(w, p[0], p[1], 0); done {
			break
		}
	}
	for _, s := range []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize"} {
		if !hasSuffix(w, s) {
			continue
		}
		base := trimSuffix(w, s)
		if s == "ion" && (len(base) == 0 || (base[len(base)-1] != 's' && base[len(base)-1] != 't')) {
			break
		}
		if measure(base) > 1 {
			w = base
		}
		break
	}
	if hasSuffix(w, "e") {
		base := trimSuffix(w, "e")
		if m := measure(base); m > 1 || m == 1 && !cvc(base) {
			w = base
		}
	}
	if measure(w) > 1 && doubleConsonant(w) && hasSuffix(w, "l") {
		w = w[:len(w)-1]
	}
	return string(w)
}

type ftsDoc struct {
	id     int64
	fields [][]ftsToken2
	length int
}

func (d *ftsDoc) has(term string, prefix bool) int {
	n := 0
	for _, f := range d.fields {
		for _, t := range f {
			if prefix && (strings.HasPrefix(t.raw, term) || strings.HasPrefix(t.stem, term)) || !prefix && t.stem == term {
				n++
			}
		}
	}
	return n
}

func (d *ftsDoc) phrase(words []string) bool {
	if len(words) == 0 {
		return false
	}
	for _, f := range d.fields {
		for i := 0; i+len(words) <= len(f); i++ {
			ok := true
			for j, w := range words {
				if f[i+j].stem != w {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

type ftsQuery struct {
	root  tsNode
	terms []ftsTerm
}

type ftsTerm struct {
	text   string
	prefix bool
}

func termWords(text string) []string {
	var words []string
	for _, t := range analyze(text) {
		if !stopWords[t.raw] {
			words = append(words, t.stem)
		}
	}
	return words
}

func compileQuery(match string) (*ftsQuery, error) {
	root, err := parseFTSQuery(match)
	if err != nil {
		return nil, err
	}
	q := &ftsQuery{root: root}
	q.collect(root, false)
	return q, nil
}

func (q *ftsQuery) collect(n tsNode, negated bool) {
	switch x := n.(type) {
	case tsTerm:
		if negated {
			return
		}
		if x.prefix {
			q.terms = append(q.terms, ftsTerm{text: foldAccents(strings.ToLower(x.text)), prefix: true})
			return
		}
		for _, w := range termWords(x.text) {
			q.terms = append(q.terms, ftsTerm{text: w})
		}
	case tsPhrase:
		if negated {
			return
		}
		for _, w := range termWords(x.text) {
			q.terms = append(q.terms, ftsTerm{text: w})
		}
	case tsBinary:
		q.collect(x.left, negated)
		q.collect(x.right, negated || x.op == '!')
	}
}

func (q *ftsQuery) matches(d *ftsDoc) bool { return evalNode(q.root, d) }

func evalNode(n tsNode, d *ftsDoc) bool {
	switch x := n.(type) {
	case tsTerm:
		if x.prefix {
			return d.has(foldAccents(strings.ToLower(x.text)), true) > 0
		}
		words := termWords(x.text)
		if len(words) == 0 {
			return false
		}
		for _, w := range words {
			if d.has(w, false) == 0 {
				return false
			}
		}
		return true
	case tsPhrase:
		return d.phrase(termWords(x.text))
	case tsBinary:
		switch x.op {
		case '|':
			return evalNode(x.left, d) || evalNode(x.right, d)
		case '!':
			return evalNode(x.left, d) && !evalNode(x.right, d)
		}
		return evalNode(x.left, d) && evalNode(x.right, d)
	}
	return false
}

type bm25Stats struct {
	docs int
	avg  float64
	df   []int
}

func corpusStats(q *ftsQuery, docs []*ftsDoc) bm25Stats {
	st := bm25Stats{docs: len(docs), avg: 1, df: make([]int, len(q.terms))}
	total := 0
	for _, doc := range docs {
		total += doc.length
		for i, t := range q.terms {
			if doc.has(t.text, t.prefix) > 0 {
				st.df[i]++
			}
		}
	}
	if len(docs) > 0 && total > 0 {
		st.avg = float64(total) / float64(len(docs))
	}
	return st
}

func bm25(q *ftsQuery, st bm25Stats, d *ftsDoc) float64 {
	const k1, b = 1.2, 0.75
	score := 0.0
	for i, t := range q.terms {
		tf := d.has(t.text, t.prefix)
		if tf == 0 {
			continue
		}
		df := float64(st.df[i])
		idf := math.Log(1 + (float64(st.docs)-df+0.5)/(df+0.5))
		score += idf * float64(tf) * (k1 + 1) / (float64(tf) + k1*(1-b+b*float64(d.length)/st.avg))
	}
	return score
}
