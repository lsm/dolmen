package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

var (
	knownTableKeys = jsonKeys(reflect.TypeOf(schema.TableSchema{}))
	knownFieldKeys = jsonKeys(reflect.TypeOf(schema.Field{}))
)

func jsonKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = t.Field(i).Name
		}
		out[name] = true
	}
	return out
}

func encodeSchemaOver(ctx context.Context, db rowQuerier, table string, sc *schema.TableSchema, renames map[string]string) (string, error) {
	raw, err := json.Marshal(sc)
	if err != nil {
		return "", err
	}
	var prev string
	err = db.QueryRowContext(ctx, `SELECT schema_json FROM _dolmen_tables WHERE name = ?`, table).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return string(raw), nil
	}
	if err != nil {
		return "", err
	}
	return MergeUnknownSchemaKeys(prev, raw, renames)
}

func RenamesOf(changes any) map[string]string {
	cs, ok := changes.([]schema.Change)
	if !ok {
		return nil
	}
	origin := map[string]string{}
	for _, c := range cs {
		switch c.Op {
		case schema.OpDropField:
			origin[c.Name] = ""
		case schema.OpRenameField:
			src, known := origin[c.From]
			if !known {
				src = c.From
			}
			delete(origin, c.From)
			origin[c.To] = src
		}
	}
	return origin
}

func MergeUnknownSchemaKeys(prev string, next []byte, renames map[string]string) (string, error) {
	var old, cur map[string]json.RawMessage
	if err := json.Unmarshal([]byte(prev), &old); err != nil {
		return string(next), nil
	}
	if err := json.Unmarshal(next, &cur); err != nil {
		return "", err
	}
	changed := false
	for k, v := range old {
		if !knownTableKeys[k] {
			cur[k] = v
			changed = true
		}
	}
	var oldFields, curFields []map[string]json.RawMessage
	if json.Unmarshal(old["fields"], &oldFields) == nil && json.Unmarshal(cur["fields"], &curFields) == nil {
		byName := map[string]map[string]json.RawMessage{}
		for _, f := range oldFields {
			var name string
			if json.Unmarshal(f["name"], &name) == nil {
				byName[name] = f
			}
		}
		fieldsChanged := false
		for _, f := range curFields {
			var name string
			if json.Unmarshal(f["name"], &name) != nil {
				continue
			}
			source := name
			if was, ok := renames[name]; ok {
				if was == "" {
					continue
				}
				source = was
			}
			for k, v := range byName[source] {
				if !knownFieldKeys[k] {
					f[k] = v
					fieldsChanged = true
				}
			}
		}
		if fieldsChanged {
			rawFields, err := json.Marshal(curFields)
			if err != nil {
				return "", err
			}
			cur["fields"] = rawFields
			changed = true
		}
	}
	if !changed {
		return string(next), nil
	}
	out, err := json.Marshal(cur)
	return string(out), err
}
