// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode rejects duplicate keys and non-integer numbers before decoding a typed
// request. Standard encoding/json alone accepts duplicate and case-folded keys.
func Decode(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxInput*4 || !utf8.Valid(data) {
		return problem(2, "InvalidInput")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := jsonValue(decoder, 0)
	if err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return problem(2, "InvalidInput")
	}
	if err := exactFields(value, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return problem(2, "InvalidInput")
	}
	return nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, problem(2, "InvalidInput")
	}
	token, err := d.Token()
	if err != nil {
		return nil, problem(2, "InvalidInput")
	}
	switch t := token.(type) {
	case json.Number:
		if _, err := strconv.ParseInt(string(t), 10, 64); err != nil {
			return nil, problem(2, "InvalidInput")
		}
		return t, nil
	case json.Delim:
		if t == '{' {
			m := map[string]any{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, problem(2, "InvalidInput")
				}
				key, ok := k.(string)
				if !ok {
					return nil, problem(2, "InvalidInput")
				}
				if _, exists := m[key]; exists {
					return nil, problem(2, "InvalidInput")
				}
				v, err := jsonValue(d, depth+1)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			if end, err := d.Token(); err != nil || end != json.Delim('}') {
				return nil, problem(2, "InvalidInput")
			}
			return m, nil
		}
		if t == '[' {
			a := []any{}
			for d.More() {
				v, err := jsonValue(d, depth+1)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return nil, problem(2, "InvalidInput")
			}
			return a, nil
		}
		return nil, problem(2, "InvalidInput")
	default:
		return token, nil
	}
}

func exactFields(value any, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() || value == nil {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		m, ok := value.(map[string]any)
		if !ok {
			return problem(2, "InvalidInput")
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key != "-" && f.IsExported() {
				if key == "" {
					key = f.Name
				}
				fields[key] = f.Type
			}
		}
		for k, v := range m {
			f, ok := fields[k]
			if !ok {
				return problem(2, "InvalidInput")
			}
			if err := exactFields(v, f); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if a, ok := value.([]any); ok {
			for _, v := range a {
				if err := exactFields(v, typ.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if m, ok := value.(map[string]any); ok {
			for _, v := range m {
				if err := exactFields(v, typ.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func encode(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, problem(2, "InvalidInput")
	}
	return append(data, '\n'), nil
}
