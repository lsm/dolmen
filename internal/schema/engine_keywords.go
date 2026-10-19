package schema

import "fmt"

var engineKeywords = map[string]map[string]bool{
	"postgresql": {
		"analyse":         true,
		"any":             true,
		"array":           true,
		"asymmetric":      true,
		"authorization":   true,
		"binary":          true,
		"both":            true,
		"collation":       true,
		"concurrently":    true,
		"current_catalog": true,
		"current_role":    true,
		"current_schema":  true,
		"current_user":    true,
		"false":           true,
		"fetch":           true,
		"freeze":          true,
		"grant":           true,
		"ilike":           true,
		"lateral":         true,
		"leading":         true,
		"localtime":       true,
		"localtimestamp":  true,
		"only":            true,
		"overlaps":        true,
		"placing":         true,
		"session_user":    true,
		"similar":         true,
		"some":            true,
		"symmetric":       true,
		"system_user":     true,
		"tablesample":     true,
		"trailing":        true,
		"true":            true,
		"user":            true,
		"variadic":        true,
		"verbose":         true,
	},
	"duckdb": {
		"analyse":       true,
		"anti":          true,
		"any":           true,
		"array":         true,
		"asof":          true,
		"asymmetric":    true,
		"at":            true,
		"authorization": true,
		"binary":        true,
		"both":          true,
		"collation":     true,
		"concurrently":  true,
		"describe":      true,
		"false":         true,
		"fetch":         true,
		"freeze":        true,
		"ilike":         true,
		"lambda":        true,
		"lateral":       true,
		"leading":       true,
		"only":          true,
		"overlaps":      true,
		"pivot":         true,
		"pivot_longer":  true,
		"pivot_wider":   true,
		"placing":       true,
		"positional":    true,
		"qualify":       true,
		"semi":          true,
		"show":          true,
		"similar":       true,
		"some":          true,
		"summarize":     true,
		"symmetric":     true,
		"tablesample":   true,
		"trailing":      true,
		"true":          true,
		"unpack":        true,
		"unpivot":       true,
		"variadic":      true,
		"verbose":       true,
	},
}

var engineLabels = map[string]string{"postgresql": "PostgreSQL", "duckdb": "DuckDB, which the lakehouse engine queries with"}

func ValidateEngineIdent(dialect, name, what string) error {
	words := engineKeywords[dialect]
	if !words[name] {
		return nil
	}
	suggestion := "a different name"
	for _, cand := range []string{"my_" + name, name + "_field"} {
		if ValidIdent(cand) && !words[cand] {
			suggestion = cand
			break
		}
	}
	return fmt.Errorf("%s %q is a reserved keyword in %s, so a query could name it only in double quotes; use a different name such as %q", what, name, engineLabels[dialect], suggestion)
}
