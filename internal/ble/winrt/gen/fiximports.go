//go:build ignore

// fiximports points the generated bindings at each other instead of at upstream winrt-go, whose
// windows/devices packages hold fewer methods. With -clean it removes the previous output first.
package main

import (
	"bytes"
	"flag"
	"go/format"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	upstream = `"github.com/saltosystems/winrt-go/windows/devices/`
	local    = `"btproxy/internal/ble/winrt/gen/windows/devices/`
)

func main() {
	clean := flag.Bool("clean", false, "remove the generated tree")
	flag.Parse()
	if *clean {
		if err := os.RemoveAll("windows"); err != nil {
			log.Fatal(err)
		}
		return
	}
	err := filepath.WalkDir("windows", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		nb := bytes.ReplaceAll(b, []byte(upstream), []byte(local))
		if bytes.Equal(nb, b) {
			return nil
		}
		if nb, err = format.Source(nb); err != nil { // the moved imports sort differently
			return err
		}
		return os.WriteFile(path, nb, 0o644)
	})
	if err != nil {
		log.Fatal(err)
	}
}
