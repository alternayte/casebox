package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// object is a JSON object that keeps its key order, so rewriting a person's settings file
// changes only the hook entries Casebox owns.
type object struct {
	keys   []string
	values map[string]json.RawMessage
}

func newObject() *object { return &object{values: map[string]json.RawMessage{}} }

func (o *object) UnmarshalJSON(data []byte) error {
	o.keys, o.values = nil, map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, dup := o.values[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.values[key] = raw
	}
	_, err = dec.Token()
	return err
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		b.Write(o.values[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *object) get(key string, into any) (bool, error) {
	raw, ok := o.values[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, into)
}

func (o *object) set(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = raw
	return nil
}

func (o *object) remove(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// indent formats a document with two-space indentation and a final newline.
func indent(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}
