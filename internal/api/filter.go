package api

import (
	"strings"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
)

func (s *Server) checkFilter(sc *schema.TableSchema, expr string, args []any) error {
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
