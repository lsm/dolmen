package auth

import (
	"fmt"
	"strings"
)

type Verb string

const RowAccessOwn = "own"

const (
	VerbCreate Verb = "create"
	VerbRead   Verb = "read"
	VerbUpdate Verb = "update"
	VerbDelete Verb = "delete"
	VerbSchema Verb = "schema"
	VerbAdmin  Verb = "admin"
)

var VerbOrder = []Verb{VerbCreate, VerbRead, VerbUpdate, VerbDelete, VerbSchema, VerbAdmin}

type VerbSet uint8

func (v Verb) bit() (VerbSet, bool) {
	for i, known := range VerbOrder {
		if known == v {
			return 1 << uint(i), true
		}
	}
	return 0, false
}

func ParseVerb(raw string) (Verb, error) {
	v := Verb(raw)
	if _, ok := v.bit(); !ok {
		return "", fmt.Errorf("unknown verb %q: the verbs are %s", raw, VerbList())
	}
	return v, nil
}

func VerbList() string {
	names := make([]string, 0, len(VerbOrder))
	for _, v := range VerbOrder {
		names = append(names, string(v))
	}
	return strings.Join(names, ", ")
}

func NewVerbSet(verbs ...Verb) VerbSet {
	var set VerbSet
	for _, v := range verbs {
		if bit, ok := v.bit(); ok {
			set |= bit
		}
	}
	return set
}

func (s VerbSet) Has(v Verb) bool {
	bit, ok := v.bit()
	return ok && s&bit != 0
}

func (s VerbSet) HasAll(verbs ...Verb) bool {
	for _, v := range verbs {
		if !s.Has(v) {
			return false
		}
	}
	return true
}

func (s VerbSet) HasAny(verbs ...Verb) bool {
	for _, v := range verbs {
		if s.Has(v) {
			return true
		}
	}
	return false
}

func (s VerbSet) Empty() bool { return s == 0 }

func (s VerbSet) List() []Verb {
	out := make([]Verb, 0, len(VerbOrder))
	for _, v := range VerbOrder {
		if s.Has(v) {
			out = append(out, v)
		}
	}
	return out
}

func (s VerbSet) Strings() []string {
	verbs := s.List()
	out := make([]string, len(verbs))
	for i, v := range verbs {
		out[i] = string(v)
	}
	return out
}

func ParseVerbs(raw []string) (VerbSet, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("verbs is required and must list at least one of %s", VerbList())
	}
	var set VerbSet
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		if _, dup := seen[r]; dup {
			return 0, fmt.Errorf("verbs lists %q twice: each verb may appear once", r)
		}
		seen[r] = struct{}{}
		v, err := ParseVerb(r)
		if err != nil {
			return 0, err
		}
		bit, _ := v.bit()
		set |= bit
	}
	return set, nil
}
