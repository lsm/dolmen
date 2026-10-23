package constraints

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/value"
)

type scalar struct {
	kind   schema.FieldType
	text   string
	number *big.Rat
}

func coerceScalar(field schema.Field, input any) (scalar, error) {
	if field.Type == schema.Boolean {
		if n, ok := input.(int64); ok && (n == 0 || n == 1) {
			input = n == 1
		}
	}
	v, err := value.Coerce(field, input)
	if err != nil || v == nil {
		return scalar{}, fmt.Errorf("constraint field %q has an invalid scalar value; use a value matching its declared type", field.Name)
	}
	s := scalar{kind: field.Type}
	switch field.Type {
	case schema.String, schema.Text, schema.Timestamp:
		s.text = v.(string)
		if field.Type == schema.Text {
			s.kind = schema.String
		}
	case schema.Number:
		s.number = new(big.Rat)
		switch n := v.(type) {
		case int64:
			s.number.SetInt64(n)
		case float64:
			if s.number.SetFloat64(n) == nil {
				return scalar{}, fmt.Errorf("constraint field %q must hold a finite number", field.Name)
			}
		default:
			return scalar{}, fmt.Errorf("constraint field %q has an unsupported numeric representation", field.Name)
		}
		s.text = s.number.RatString()
	case schema.Boolean:
		if v.(int64) == 1 {
			s.text = "1"
		} else {
			s.text = "0"
		}
	default:
		return scalar{}, fmt.Errorf("constraint field %q must be string, text, number, boolean or timestamp", field.Name)
	}
	return s, nil
}

func appendPart(dst []byte, text string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(text)))
	return append(dst, text...)
}

func UniqueKey(fields []schema.Field, row map[string]any, owner *string) (string, bool, error) {
	if len(fields) == 0 || len(fields) > schema.MaxUniqueConstraintFields {
		return "", false, fmt.Errorf("a unique key requires one to %d declared scalar fields", schema.MaxUniqueConstraintFields)
	}
	key := []byte{0}
	if owner != nil {
		key[0] = 1
		key = appendPart(key, *owner)
	}
	null := false
	for _, field := range fields {
		input := row[field.Name]
		if input == nil {
			null = true
			continue
		}
		s, err := coerceScalar(field, input)
		if err != nil {
			return "", false, err
		}
		key = appendPart(key, string(s.kind))
		key = appendPart(key, s.text)
	}
	if null {
		return "", false, nil
	}
	return string(key), true, nil
}

func SameOwner(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func ReferenceID(input any) (int64, bool, error) {
	if input == nil {
		return 0, false, nil
	}
	v, err := value.Coerce(schema.Field{Name: "reference", Type: schema.Number}, input)
	if err != nil {
		return 0, false, fmt.Errorf("a foreign key must hold a positive int64 system ID or NULL")
	}
	switch n := v.(type) {
	case int64:
		if n > 0 {
			return n, true, nil
		}
	case float64:
		if n >= 1 && n < 0x1p63 && math.Trunc(n) == n {
			return int64(n), true, nil
		}
	}
	return 0, false, fmt.Errorf("a foreign key must hold a positive int64 system ID or NULL")
}
