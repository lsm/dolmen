package value

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

func Decode(t schema.FieldType, v any) any {
	if v == nil {
		return nil
	}
	switch t {
	case schema.Secret:
		return secret.Mask
	case schema.Boolean:
		switch b := v.(type) {
		case bool:
			return b
		case int64:
			if b == 0 {
				return false
			}
			if b == 1 {
				return true
			}
		}
		return v
	case schema.JSON:
		s, ok := v.(string)
		if !ok {
			if raw, isBytes := v.([]byte); isBytes {
				s, ok = string(raw), true
			}
		}
		if ok {
			var out any
			dec := json.NewDecoder(strings.NewReader(s))
			dec.UseNumber()
			if err := dec.Decode(&out); err == nil {
				return out
			}
		}
		return v
	case schema.Vector:
		if raw, ok := v.([]byte); ok {
			if fv, err := schema.DecodeVector(raw); err == nil {
				out := make([]float64, len(fv))
				for i, x := range fv {
					out[i] = float64(x)
				}
				return out
			}
		}
		return Normalize(v)
	}
	return Normalize(v)
}

func Normalize(v any) any {
	if b, ok := v.([]byte); ok {
		return base64.StdEncoding.EncodeToString(b)
	}
	return v
}
