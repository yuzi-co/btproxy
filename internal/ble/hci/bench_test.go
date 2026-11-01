package hci

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// BenchmarkDecoder decodes non-scannable advertisements, the common case, and scannable ones with their scan
// responses during an active scan.
func BenchmarkDecoder(b *testing.B) {
	ev := func(s string) []byte {
		e, _ := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
		return e
	}
	for _, c := range []struct {
		name   string
		active bool
		events [][]byte
	}{
		{"nonconn", false, [][]byte{ev(evExtNonconn)}},
		{"scanrsp", true, [][]byte{ev(evAdvInd), ev(evScanRsp)}},
	} {
		b.Run(c.name, func(b *testing.B) {
			d := decoder{emit: func(ble.Advert) {}, active: c.active}
			now := time.Unix(1000, 0)
			b.ReportAllocs()
			for b.Loop() {
				for _, e := range c.events {
					d.event(e, now)
				}
			}
		})
	}
}
