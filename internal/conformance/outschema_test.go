package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type schemaCheck struct {
	components map[string]any
	problems   []string
}

func (c *schemaCheck) fail(path, format string, args ...any) {
	c.problems = append(c.problems, path+": "+fmt.Sprintf(format, args...))
}

func (c *schemaCheck) resolve(s map[string]any) map[string]any {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		next, ok := c.components[name].(map[string]any)
		if !ok {
			c.fail(ref, "unresolvable reference")
			return map[string]any{}
		}
		s = next
	}
}

func jsonTypeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func typeAllows(want any, got string) bool {
	var names []string
	switch w := want.(type) {
	case string:
		names = []string{w}
	case []any:
		for _, n := range w {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
	default:
		return true
	}
	for _, n := range names {
		if n == got || n == "number" && got == "integer" {
			return true
		}
	}
	return false
}

func (c *schemaCheck) check(path string, s map[string]any, v any) {
	s = c.resolve(s)
	if alts, ok := s["anyOf"].([]any); ok && !c.matchesOne(path, alts, v) {
		c.fail(path, "matches none of the anyOf alternatives: %s", compact(v))
		return
	}
	if want, ok := s["type"]; ok && !typeAllows(want, jsonTypeOf(v)) {
		c.fail(path, "is %s, want %v", jsonTypeOf(v), want)
		return
	}
	if want, ok := s["const"]; ok && !reflect.DeepEqual(normalizeJSON(want), v) {
		c.fail(path, "is %s, want const %s", compact(v), compact(want))
	}
	if enum, ok := s["enum"]; ok {
		found := false
		for _, e := range normalizeJSON(enum).([]any) {
			if reflect.DeepEqual(e, v) {
				found = true
			}
		}
		if !found {
			c.fail(path, "is %s, outside enum %s", compact(v), compact(enum))
		}
	}
	switch x := v.(type) {
	case float64:
		if min, ok := number(s["minimum"]); ok && x < min {
			c.fail(path, "is %v, below minimum %v", x, min)
		}
		if max, ok := number(s["maximum"]); ok && x > max {
			c.fail(path, "is %v, above maximum %v", x, max)
		}
	case string:
		if min, ok := number(s["minLength"]); ok && float64(len([]rune(x))) < min {
			c.fail(path, "is shorter than minLength %v", min)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			c.fail(path, "is %q, which does not match %s", x, p)
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				c.check(fmt.Sprintf("%s[%d]", path, i), items, e)
			}
		}
		if s["uniqueItems"] == true {
			for i := range x {
				for j := i + 1; j < len(x); j++ {
					if reflect.DeepEqual(x[i], x[j]) {
						c.fail(path, "repeats %s", compact(x[i]))
					}
				}
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"]; ok {
			for _, name := range normalizeJSON(req).([]any) {
				if _, present := x[name.(string)]; !present {
					c.fail(path, "lacks required %q", name)
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				c.check(path+"."+k, ps, x[k])
				continue
			}
			switch extra := s["additionalProperties"].(type) {
			case bool:
				if !extra {
					c.fail(path, "carries %q, which the schema does not declare and forbids", k)
				}
			case map[string]any:
				c.check(path+"."+k, extra, x[k])
			}
		}
	}
}

func (c *schemaCheck) matchesOne(path string, alts []any, v any) bool {
	for _, alt := range alts {
		sub := &schemaCheck{components: c.components}
		sub.check(path, alt.(map[string]any), v)
		if len(sub.problems) == 0 {
			return true
		}
	}
	return false
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func normalizeJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func compact(v any) string {
	raw, _ := json.Marshal(v)
	if len(raw) > 120 {
		return string(raw[:120]) + "…"
	}
	return string(raw)
}

func (h *harness) checkOutputSchema(t *testing.T, op string, data any) {
	t.Helper()
	def, ok := h.api.Op(op)
	if !ok || def.OutputSchema == nil {
		return
	}
	if h.schemaComponents == nil {
		doc := normalizeJSON(h.api.OpenAPIDoc(h.srv.URL)).(map[string]any)
		h.schemaComponents, _ = doc["components"].(map[string]any)["schemas"].(map[string]any)
	}
	c := &schemaCheck{components: h.schemaComponents}
	c.check(op, normalizeJSON(def.OutputSchema).(map[string]any), normalizeJSON(data))
	for _, p := range c.problems {
		t.Errorf("%s answered outside its published output schema: %s", op, p)
	}
}
