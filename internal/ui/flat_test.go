package ui

import (
	"maps"
	"testing"
)

func TestFlatRoundTrip(t *testing.T) {
	m := map[string]string{"name": "btproxy-x", "q": `a"b\c`, "ctl": "line\nnext\ttab\x01", "uni": "Kühlschrank ✓", "html": "<b>&", "": "empty key"}
	got, err := DecodeFlat(EncodeFlat(m))
	if err != nil || !maps.Equal(got, m) {
		t.Fatalf("%v %v\n%s", got, err, EncodeFlat(m))
	}
	if s := string(EncodeFlat(map[string]string{})); s != "{}\n" {
		t.Fatalf("%q", s)
	}
}

func TestDecodeFlat(t *testing.T) {
	got, err := DecodeFlat([]byte(` { "connections" : 3, "mdns": false, "x": null, "f": -1.5e3, "u": "é😀\/" } `))
	want := map[string]string{"connections": "3", "mdns": "false", "x": "", "f": "-1.5e3", "u": "é😀/"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{``, `[]`, `{`, `{"a"}`, `{"a":}`, `{"a":"b",}`, `{"a":{}}`, `{"a":[1]}`, `{"a":"b"} x`,
		`{"a":"\x"}`, `{"a":"\u12"}`, "{\"a\":\"\n\"}", `{"a":"b"`, `{"a":tru}`, `{a:"b"}`} {
		if _, err := DecodeFlat([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
