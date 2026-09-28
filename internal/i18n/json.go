package i18n

import (
	"bytes"
	"encoding/json"
)

// dataKeys — fields that hold names, paths and values, never prose: left
// alone, so a dataset or property value can never be "translated".
var dataKeys = map[string]bool{
	"name": true, "pool": true, "dataset": true, "dev": true, "path": true,
	"mountpoint": true, "value": true, "target": true, "source": true,
	"serial": true, "model": true, "id": true, "user": true, "snapshot": true,
	"full": true, "origin": true, "vdev": true, "group": true, "original": true,
	"trashed": true, "dest_dataset": true, "host": true, "endpoint": true,
	"display_name": true, "email": true, "label": true, "schedule": true,
	"retention": true, "kind": true, "level": true, "state": true, "type": true,
	"property": true, "public_key": true, "token": true, "url": true,
}

// JSON translates the prose inside a JSON document. ok is false when b is
// not JSON (it is then returned untouched).
func JSON(b []byte) (out []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // integers (bytes) stay exact
	var v any
	if err := dec.Decode(&v); err != nil {
		return b, false
	}
	v = walk(v, "")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return b, false
	}
	return buf.Bytes(), true
}

func walk(v any, key string) any {
	switch x := v.(type) {
	case string:
		if dataKeys[key] {
			return x
		}
		return English(x)
	case []any:
		for i := range x {
			x[i] = walk(x[i], key)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = walk(e, k)
		}
		return x
	}
	return v
}
