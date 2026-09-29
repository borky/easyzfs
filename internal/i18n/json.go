package i18n

import (
	"bytes"
	"encoding/json"
	"strings"
)

// proseKeys — the fields that carry text the server wrote for people to
// read. Only these are translated: everything else (names, paths, commands,
// cron lines, zfs output, property values) is data, and a Spanish phrase in
// a file or dataset name must come back exactly as it is.
var proseKeys = map[string]bool{
	"message": true, "error": true, "warnings": true, "reason": true,
	"detail": true, "text": true, "title": true, "summary": true,
	"instructions": true, "lines": true, "version": true, "remote_version": true,
	"applyRefused": true,
}

func proseKey(k string) bool {
	return proseKeys[k] || strings.HasSuffix(k, "_reason") || strings.HasSuffix(k, "_error") ||
		strings.HasSuffix(k, "_detail") || strings.HasSuffix(k, "_result")
}

// JSON translates the prose inside a JSON document into Spanish. ok is false when b is
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
		if !proseKey(key) {
			return x
		}
		return Spanish(x)
	case []any:
		for i := range x {
			x[i] = walk(x[i], key) // a list of prose ("warnings") keeps its key
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
