package api

import (
	"strings"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// TestAdvFilter checks that the server batches only the advertisements the filter allows and reports the
// filter in the configuration dump.
func TestAdvFilter(t *testing.T) {
	f, err := ble.ParseAdvFilter("", "AA:BB:CC", false)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Name: "t", AdvFilter: f}, testAdapter{}, logx.Discard())
	s.onAdvert(ble.Advert{Addr: 0xAABBCC000001, RSSI: -60})
	if s.pendingN != 0 {
		t.Fatal("denied advertisement batched")
	}
	s.onAdvert(ble.Advert{Addr: 0xAABBCD000001, RSSI: -60})
	if s.pendingN != 1 {
		t.Fatal("allowed advertisement not batched")
	}
	if d := s.configDump(); !strings.Contains(d, "Advertisement filter: allow -; deny AA:BB:CC:*") {
		t.Errorf("config dump:\n%s", d)
	}
	if s.flushT != nil {
		s.flushT.Stop()
	}
}
