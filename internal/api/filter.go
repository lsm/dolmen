package api

import (
	"strings"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
)

func (s *Server) checkFilter(sc *schema.TableSchema, expr string, args []any) error {
	if err := scalarArgs(args); err != nil {
		return err
	}
	if !s.authn.On() || sc == nil || strings.TrimSpace(expr) == "" {
		return nil
	}
	if err := filter.Validate(expr, filter.Options{Columns: filterColumns(sc), Args: args}); err != nil {
		return badRequest("filter: %s", err.Error())
	}
	return nil
}

func filterColumns(sc *schema.TableSchema) []string {
	cols := make([]string, 0, len(sc.Fields)+3)
	cols = append(cols, "id", "created_at")
	for _, f := range sc.Fields {
		cols = append(cols, f.Name)
	}
	if sc.HasOwner {
		cols = append(cols, schema.OwnerColumn)
	}
	return cols
}

func scalarArgs(args []any) error {
	for i, arg := range args {
		switch arg.(type) {
		case []any, map[string]any:
			return badRequest("args[%d] is %s, but a bound argument must be a string, number, boolean or null; pass a JSON value as a string and parse it in SQL", i, jsonKind(arg))
		}
	}
	return nil
}

func jsonKind(v any) string {
	if _, ok := v.([]any); ok {
		return "an array"
	}
	return "an object"
}
