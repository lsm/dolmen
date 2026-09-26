package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

type fieldRef struct {
	typ reflect.Type
}

type fieldIndex struct {
	byName map[string]fieldRef
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	numberType     = reflect.TypeOf(json.Number(""))
	fieldIndexes   sync.Map
)

func fieldIndexOf(t reflect.Type) *fieldIndex {
	if cached, ok := fieldIndexes.Load(t); ok {
		return cached.(*fieldIndex)
	}
	idx := &fieldIndex{byName: map[string]fieldRef{}}
	collectFields(t, idx.byName)
	fieldIndexes.Store(t, idx)
	return idx
}

func collectFields(t reflect.Type, into map[string]fieldRef) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Anonymous && name == "" {
			if ft.Kind() == reflect.Struct && ft != rawMessageType {
				collectFields(ft, into)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		if _, taken := into[name]; !taken {
			into[name] = fieldRef{typ: ft}
		}
	}
}

func rejectUnknownKeys(probe *jsonObject, v any) error {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return checkKeys(probe, t)
}

func checkKeys(probe any, t reflect.Type) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t == rawMessageType || t == numberType || t.Kind() == reflect.Interface || t.Kind() == reflect.Map {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := probe.(*jsonObject)
		if !ok {
			return nil
		}
		idx := fieldIndexOf(t)
		for _, key := range obj.keys {
			field, known := idx.byName[key]
			if !known {
				return &unknownFieldError{Field: key}
			}
			val, _ := obj.get(key)
			if err := checkKeys(val, field.typ); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		elems, ok := probe.([]any)
		if !ok {
			return nil
		}
		for _, e := range elems {
			if err := checkKeys(e, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
