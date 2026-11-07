package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The settings file is a flat JSON object of flag values. The page saves strings, but a file written by hand
// may give numbers, booleans or null, which read as their literal text ("3", "true") and "".

// parseConfig reads a settings file. Nested objects and arrays are rejected.
func parseConfig(data []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		switch {
		case len(v) > 0 && v[0] == '"':
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, err
			}
			m[k] = s
		case bytes.Equal(v, []byte("null")):
			m[k] = ""
		case len(v) > 0 && (v[0] == '{' || v[0] == '['):
			return nil, fmt.Errorf("json: %q: expected a string, number, boolean or null", k)
		default:
			m[k] = string(v)
		}
	}
	return m, nil
}

// formatConfig writes m as parseConfig reads it: sorted keys, one per line.
func formatConfig(m map[string]string) []byte {
	b, _ := json.MarshalIndent(m, "", "  ") // cannot fail for a map of strings
	return append(b, '\n')
}
