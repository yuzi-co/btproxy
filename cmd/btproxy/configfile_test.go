package main

import (
	"maps"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	m := map[string]string{"name": "btproxy-x", "q": `a"b\c`, "ctl": "line\nnext\ttab\x01", "uni": "Kühlschrank ✓", "html": "<b>&", "": "empty key"}
	got, err := parseConfig(formatConfig(m))
	if err != nil || !maps.Equal(got, m) {
		t.Fatalf("%v %v\n%s", got, err, formatConfig(m))
	}
	if s := string(formatConfig(map[string]string{"b": "2", "a": "1"})); s != "{\n  \"a\": \"1\",\n  \"b\": \"2\"\n}\n" {
		t.Fatalf("%q", s)
	}
	if s := string(formatConfig(map[string]string{})); s != "{}\n" {
		t.Fatalf("%q", s)
	}
}

// A file written by hand may hold numbers, booleans and null, as the README shows.
func TestParseConfig(t *testing.T) {
	got, err := parseConfig([]byte(` { "connections" : 3, "mdns": false, "x": null, "f": -1.5e3, "u": "é😀\/" } `))
	want := map[string]string{"connections": "3", "mdns": "false", "x": "", "f": "-1.5e3", "u": "é😀/"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{``, `[]`, `{`, `{"a"}`, `{"a":}`, `{"a":"b",}`, `{"a":{}}`, `{"a":[1]}`, `{"a":"b"} x`,
		`{"a":"\x"}`, `{"a":"\u12"}`, "{\"a\":\"\n\"}", `{"a":"b"`, `{"a":tru}`, `{a:"b"}`} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
