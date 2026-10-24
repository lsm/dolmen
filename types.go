package dolmen

import (
	"github.com/lsm/dolmen/internal/derr"
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
	File      FieldType = "file"
)

type Field struct {
	Name       string
	Type       FieldType
	Fulltext   bool
	Vectorize  bool
	Dim        int
	Required   bool
	Enum       []string
	Shape      string
	Default    any
	MaxBytes   int64
	MediaTypes []string
}

type TableSchema struct {
	Namespace        string
	Name             string
	Version          int
	Fields           []Field
	EmbedSpace       string
	EmbedDim         int
	Constraints      []Constraint
	AcceptReferences bool
}

type Constraint interface {
	constraintName() string
}

type UniqueConstraint struct {
	Name   string
	Fields []string
	Where  string
}

type ForeignKeyConstraint struct {
	Name            string
	Field           string
	ReferencesTable string
}

func (c UniqueConstraint) constraintName() string { return c.Name }

func (c ForeignKeyConstraint) constraintName() string { return c.Name }

type CreateTableOptions struct {
	Constraints      []Constraint
	AcceptReferences bool
}

func constraintsToSchema(cs []Constraint) ([]schema.Constraint, error) {
	out := make([]schema.Constraint, 0, len(cs))
	for i, c := range cs {
		switch c := c.(type) {
		case UniqueConstraint:
			out = append(out, schema.Constraint{Name: c.Name, Kind: schema.ConstraintUnique, Fields: c.Fields, Where: c.Where})
		case ForeignKeyConstraint:
			out = append(out, schema.Constraint{Name: c.Name, Kind: schema.ConstraintForeignKey, Field: c.Field, References: &schema.ConstraintReference{Table: c.ReferencesTable, Field: "id"}})
		default:
			return nil, derr.New(derr.InvalidRequest, "constraints[%d]: use a dolmen.UniqueConstraint or dolmen.ForeignKeyConstraint value", i)
		}
	}
	return out, nil
}

func schemaToConstraints(cs []schema.Constraint) []Constraint {
	if len(cs) == 0 {
		return nil
	}
	out := make([]Constraint, 0, len(cs))
	for _, c := range cs {
		if c.Kind == schema.ConstraintForeignKey && c.References != nil {
			out = append(out, ForeignKeyConstraint{Name: c.Name, Field: c.Field, ReferencesTable: c.References.Table})
			continue
		}
		out = append(out, UniqueConstraint{Name: c.Name, Fields: c.Fields, Where: c.Where})
	}
	return out
}

func fieldsToSchema(fields []Field) []schema.Field {
	if fields == nil {
		return nil
	}
	out := make([]schema.Field, len(fields))
	for i, f := range fields {
		out[i] = schema.Field{
			Name:       f.Name,
			Type:       schema.FieldType(f.Type),
			Fulltext:   f.Fulltext,
			Vectorize:  f.Vectorize,
			Dim:        f.Dim,
			Required:   f.Required,
			Enum:       f.Enum,
			Shape:      f.Shape,
			Default:    f.Default,
			MaxBytes:   f.MaxBytes,
			MediaTypes: f.MediaTypes,
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
			Name:       f.Name,
			Type:       FieldType(f.Type),
			Fulltext:   f.Fulltext,
			Vectorize:  f.Vectorize,
			Dim:        f.Dim,
			Required:   f.Required,
			Enum:       f.Enum,
			Shape:      f.Shape,
			Default:    f.Default,
			MaxBytes:   f.MaxBytes,
			MediaTypes: f.MediaTypes,
		}
	}
	return out
}

func schemaToTableSchema(sc *schema.TableSchema) TableSchema {
	if sc == nil {
		return TableSchema{}
	}
	return TableSchema{
		Namespace:        sc.Namespace,
		Name:             sc.Name,
		Version:          sc.Version,
		Fields:           schemaToFields(sc.Fields),
		EmbedSpace:       sc.EmbedSpace,
		EmbedDim:         sc.EmbedDim,
		Constraints:      schemaToConstraints(sc.Constraints),
		AcceptReferences: sc.AcceptReferences,
	}
}
