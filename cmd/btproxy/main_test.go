package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptionKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32) // a test key
	b64 := base64.StdEncoding.EncodeToString(raw)
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("  "+b64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, flag, file, env string
		want                  []byte
		err                   bool
	}{
		{name: "none"},
		{name: "flag", flag: b64, want: raw},
		{name: "file", file: file, want: raw},
		{name: "env", env: b64, want: raw},
		{name: "flag over env", flag: b64, env: "bad", want: raw},
		{name: "both flags", flag: b64, file: file, err: true},
		{name: "short", flag: base64.StdEncoding.EncodeToString(raw[:16]), err: true},
		{name: "not base64", flag: "not base64!", err: true},
		{name: "missing file", file: file + ".missing", err: true},
	} {
		got, err := encryptionKey(c.flag, c.file, c.env)
		if (err != nil) != c.err || !bytes.Equal(got, c.want) {
			t.Errorf("%s: got %x, %v", c.name, got, err)
		}
	}
}
