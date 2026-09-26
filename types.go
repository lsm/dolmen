package dolmen

import (
	"github.com/lsm/dolmen/internal/schema"
)

type FieldType string

const (
	String    FieldType = "string"
	Text      FieldType = "text"
	Number    FieldType = "number"
	Boolean   FieldType = "boolean"
	Timestamp FieldType = "timestamp"
	JSON      FieldType = "json"
	Vector    FieldType = "vector"
	Secret    FieldType = "secret"
)

type Field struct {
	Name      string
	Type      FieldType
	Fulltext  bool
	Vectorize bool
	Dim       int
	Required  bool
	Enum      []string
	Shape     string
	Default   any
}

type TableSchema struct {
	Namespace  string
	Name       string
	Version    int
	Fields     []Field
	EmbedSpace string
	EmbedDim   int
}

func fieldsToSchema(fields []Field) []schema.Field {
	if fields == nil {
		return nil
	}
	out := make([]schema.Field, len(fields))
	for i, f := range fields {
		out[i] = schema.Field{
			Name:      f.Name,
			Type:      schema.FieldType(f.Type),
			Fulltext:  f.Fulltext,
			Vectorize: f.Vectorize,
			Dim:       f.Dim,
			Required:  f.Required,
			Enum:      f.Enum,
			Shape:     f.Shape,
			Default:   f.Default,
		}
	}
	return out
}

func schemaToFields(fields []schema.Field) []Field {
	if fields == nil {
		return nil
	}
	out := make([]Field, len(fields))
	for i, f := range fields {
		out[i] = Field{
			Name:      f.Name,
			Type:      FieldType(f.Type),
			Fulltext:  f.Fulltext,
			Vectorize: f.Vectorize,
			Dim:       f.Dim,
			Required:  f.Required,
			Enum:      f.Enum,
			Shape:     f.Shape,
			Default:   f.Default,
		}
	}
	return out
}

func schemaToTableSchema(sc *schema.TableSchema) TableSchema {
	if sc == nil {
		return TableSchema{}
	}
	return TableSchema{
		Namespace:  sc.Namespace,
		Name:       sc.Name,
		Version:    sc.Version,
		Fields:     schemaToFields(sc.Fields),
		EmbedSpace: sc.EmbedSpace,
		EmbedDim:   sc.EmbedDim,
	}
}
