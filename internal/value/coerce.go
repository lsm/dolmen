package value

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func finiteNumber(f float64, name string) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("field %q: number must be finite (NaN and infinities are not storable)", name)
	}
	return f, nil
}

func Coerce(f schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case schema.Number:
		switch n := v.(type) {
		case float64:
			return finiteNumber(n, f.Name)
		case float32:
			return finiteNumber(float64(n), f.Name)
		case int:
			return int64(n), nil
		case int8:
			return int64(n), nil
		case int16:
			return int64(n), nil
		case int32:
			return int64(n), nil
		case int64:
			return n, nil
		case uint:
			if uint64(n) > math.MaxInt64 {
				return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
			}
			return int64(n), nil
		case uint8:
			return int64(n), nil
		case uint16:
			return int64(n), nil
		case uint32:
			return int64(n), nil
		case uint64, uintptr:
			u := reflect.ValueOf(v).Uint()
			if u > math.MaxInt64 {
				return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
			}
			return int64(u), nil
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
			fErrName := f.Name
			f, err := n.Float64()
			if err != nil {
				return nil, fmt.Errorf("field %q: expected a number", fErrName)
			}
			return finiteNumber(f, fErrName)
		default:
			rv := reflect.ValueOf(v)
			switch rv.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return rv.Int(), nil
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				u := rv.Uint()
				if u > math.MaxInt64 {
					return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
				}
				return int64(u), nil
			case reflect.Float32, reflect.Float64:
				return finiteNumber(rv.Float(), f.Name)
			}
			return nil, fmt.Errorf("field %q: expected a number", f.Name)
		}
	case schema.Boolean:
		b, ok := v.(bool)
		if !ok {
			rv := reflect.ValueOf(v)
			if rv.Kind() != reflect.Bool {
				return nil, fmt.Errorf("field %q: expected a boolean", f.Name)
			}
			b = rv.Bool()
		}
		if b {
			return int64(1), nil
		}
		return int64(0), nil
	case schema.Vector:
		var floats []float64
		switch arr := v.(type) {
		case []any:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				switch n := x.(type) {
				case float64:
					floats[i] = n
				case int:
					floats[i] = float64(n)
				case int64:
					floats[i] = float64(n)
				case json.Number:
					fv, err := n.Float64()
					if err != nil {
						return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
					}
					floats[i] = fv
				default:
					return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
				}
			}
		case []float64:
			floats = arr
		case []float32:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				floats[i] = float64(x)
			}
		default:
			return nil, fmt.Errorf("field %q: expected an array of numbers", f.Name)
		}
		if len(floats) != f.Dim {
			return nil, fmt.Errorf("field %q: vector has %d entries, expected dim %d", f.Name, len(floats), f.Dim)
		}
		out := make([]float32, len(floats))
		for i, x := range floats {
			if math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
				return nil, fmt.Errorf("field %q: vector entry %d is outside the float32 range", f.Name, i)
			}
			out[i] = float32(x)
		}
		return schema.EncodeVector(out), nil
	case schema.JSON:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("field %q: cannot marshal JSON: %w", f.Name, err)
		}
		if err := schema.ShapeViolation(f.Name, f.Shape, b); err != nil {
			return nil, err
		}
		return string(b), nil
	case schema.Timestamp:
		s, ok := StoredString(v)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a timestamp string", f.Name)
		}
		canonical, ok := schema.CanonicalTimestamp(s)
		if !ok {
			return nil, fmt.Errorf("field %q: expected an ISO/RFC3339 timestamp, got %q", f.Name, s)
		}
		return canonical, nil
	default:
		s, ok := StoredString(v)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a string", f.Name)
		}
		if !schema.EnumAllows(f.Enum, s) {
			return nil, fmt.Errorf("field %q: value %q is not one of the allowed enum values (%s)", f.Name, s, strings.Join(f.Enum, ", "))
		}
		return s, nil
	}
}

func StoredString(v any) (string, bool) {
	rv := reflect.ValueOf(v)
	seen := map[uintptr]bool{}
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "", false
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if seen[ptr] {
				return "", false
			}
			seen[ptr] = true
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		if t, ok := rv.Interface().(time.Time); ok {
			return t.Format(time.RFC3339Nano), true
		}
		return "", false
	}
	if rv.Kind() != reflect.String {
		return "", false
	}
	return rv.String(), true
}
