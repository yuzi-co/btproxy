package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateReadme = flag.Bool("update-readme", false, "rewrite README's flag table from the settings table")

// readmeFlagTable renders the settings table as README's flag table: the same on every OS, since the defaults
// that differ by OS carry their own words.
func readmeFlagTable(table []setting) string {
	var b strings.Builder
	b.WriteString("| Flag | Default | Meaning |\n|---|---|---|\n")
	for _, s := range table {
		if s.docWith != "" {
			continue
		}
		flags := "`-" + s.name + "`"
		for _, o := range table {
			if o.docWith == s.name {
				flags += ", `-" + o.name + "`"
			}
		}
		def := s.docDefault
		if v := fmt.Sprint(s.def); def == "" && v != "" {
			def = "`" + v + "`"
		}
		doc := s.doc
		if doc == "" {
			doc = s.usage
		}
		if s.backend != "" {
			doc += " (" + backendNote(s.backend) + ")"
		}
		row := "| " + flags + " | " + def + " | " + doc + " |"
		b.WriteString(strings.ReplaceAll(row, "|  |", "| |") + "\n")
	}
	return b.String()
}

// TestReadmeFlagTable checks README's flag table against the settings table. After a change to the table,
// go test ./cmd/btproxy -run TestReadmeFlagTable -update-readme rewrites it.
func TestReadmeFlagTable(t *testing.T) {
	path := filepath.Join("..", "..", "README.md")
	readme, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(readme), "\n")
	start := -1
	for i, l := range lines {
		if l == "| Flag | Default | Meaning |" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("README has no flag table")
	}
	end := start
	for end < len(lines) && lines[end] != "" {
		end++
	}
	want := strings.Split(strings.TrimSuffix(readmeFlagTable(settingsTable()), "\n"), "\n")
	got := lines[start:end]
	if *updateReadme {
		out := append(append(append([]string{}, lines[:start]...), want...), lines[end:]...)
		if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := range max(len(got), len(want)) {
		g, w := "", ""
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Fatalf("README line %d differs from the settings table; run this test with -update-readme\n"+
				"README: %s\ntable:  %s", start+i+1, g, w)
		}
	}
}
