package winrt

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

func TestChoosePreset(t *testing.T) {
	// The presets as Windows 11 (build 26300) reports them.
	presets := []preset{
		{name: "throughput", ConnParams: ble.ConnParams{MinInterval: 12, MaxInterval: 12, Timeout: 200}},
		{name: "balanced", ConnParams: ble.ConnParams{MinInterval: 24, MaxInterval: 48, Timeout: 400}},
		{name: "power", ConnParams: ble.ConnParams{MinInterval: 72, MaxInterval: 144, Timeout: 600}},
	}
	for _, c := range []struct {
		min, max uint16
		want     string
	}{
		{6, 6, "throughput"},
		{6, 9, "throughput"},   // aioesphomeapi's fast preset (7.5-11.25 ms)
		{8, 16, "throughput"},  // overlaps throughput only
		{16, 20, "throughput"}, // 20-25 ms: nearer 15 ms than 30-60 ms
		{24, 40, "balanced"},   // ESPHome's default-ish
		{40, 80, "balanced"},   // overlaps balanced and power, balanced nearer
		{80, 100, "power"},     // inside power
		{400, 800, "power"},    // slower than any preset
		{3200, 3200, "power"},  // 4 s
		{0, 0, "throughput"},   // nonsense: fastest
		{100, 50, "power"},     // min above max
	} {
		got := presets[choosePreset(ble.ConnParams{MinInterval: c.min, MaxInterval: c.max}, presets)].name
		if got != c.want {
			t.Errorf("%d-%d: %s, want %s", c.min, c.max, got, c.want)
		}
	}
}
