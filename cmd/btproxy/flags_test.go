package main

import (
	"flag"
	"strings"
	"testing"
)

// Every flag of the program is in the settings table, and every table entry for this OS is a flag. Flags a
// build may add outside the table go in flagsOutsideTable; there are none so far (pprof.go adds no flag, and
// the test flags are on flag.CommandLine, not on the program's FlagSet).
var flagsOutsideTable = map[string]bool{}

func TestFlagsMatchTable(t *testing.T) {
	a, err := newTestApp(t)
	if err != nil {
		t.Fatal(err)
	}
	inTable := map[string]bool{}
	for _, s := range a.table {
		if inTable[s.name] {
			t.Errorf("%s is in the table twice", s.name)
		}
		inTable[s.name] = true
	}
	for _, s := range a.table {
		if s.docWith != "" && !inTable[s.docWith] {
			t.Errorf("%s shares a README row with %s, which is not in the table", s.name, s.docWith)
		}
		if !s.here() {
			delete(inTable, s.name)
		}
	}
	a.fs.VisitAll(func(f *flag.Flag) {
		if !inTable[f.Name] && !flagsOutsideTable[f.Name] {
			t.Errorf("-%s is not in the settings table", f.Name)
		}
		delete(inTable, f.Name)
	})
	for name := range inTable {
		t.Errorf("%s is in the settings table but not a flag", name)
	}
	if u := a.fs.Lookup("adapter").Usage; !strings.HasSuffix(u, " (bluez and kernel backends)") {
		t.Errorf("-adapter usage does not name its backends: %q", u)
	}
}

// The settings page lists every setting of the table that the backend uses, in the table's order; the Windows
// autostart field is the page's own.
func TestSettingsPageSections(t *testing.T) {
	a, err := newTestApp(t, "-backend", "bluez")
	if err != nil {
		t.Fatal(err)
	}
	var shown []string
	for _, sec := range (settingsPage{a}).Page().Sections {
		for _, f := range sec.Fields {
			if f.Name != "autostart" {
				shown = append(shown, f.Name)
			}
		}
	}
	var want []string
	for _, s := range a.table {
		if s.onPage() && s.forBackend("bluez") {
			want = append(want, s.name)
		}
	}
	if strings.Join(shown, " ") != strings.Join(want, " ") {
		t.Errorf("page shows %v, want %v", shown, want)
	}
}
