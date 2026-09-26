package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type jsonObject struct {
	keys []string
	vals map[string]any
}

func (o *jsonObject) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func probeObject(body []byte) (*jsonObject, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	value, err := decodeProbeValue(dec)
	if err != nil {
		return nil, badRequest("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, badRequest("unexpected trailing content after JSON body")
	}
	obj, ok := value.(*jsonObject)
	if !ok {
		tm := &typeMismatchError{Got: jsonWordOf(value)}
		return nil, &Error{Status: http.StatusBadRequest, Code: ErrCodeInvalid, Message: tm.Error(), Cause: tm}
	}
	return obj, nil
}

func decodeProbeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := &jsonObject{vals: map[string]any{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", keyTok)
			}
			val, err := decodeProbeValue(dec)
			if err != nil {
				return nil, err
			}
			if _, dup := obj.vals[key]; !dup {
				obj.keys = append(obj.keys, key)
			}
			obj.vals[key] = val
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			val, err := decodeProbeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %v", delim)
}

func jsonWordOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case *jsonObject:
		return "an object"
	}
	return "a different type"
}
