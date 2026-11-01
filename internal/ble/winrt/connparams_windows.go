package winrt

import (
	"errors"
	"fmt"
	"math"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth"
)

// Windows 11 (build 22000) lets an app ask for connection parameters, but only as one of three presets
// (BluetoothLEPreferredConnectionParameters: ThroughputOptimized, Balanced, PowerOptimized), through
// BluetoothLEDevice.RequestPreferredConnectionParameters. The request holds while its object is open and the
// app keeps the device connected; Windows weighs it against other apps' requests. SetConnParams maps the
// interval a client asks for to the preset whose interval range is nearest, and holds that request until the
// next one or the end of the connection. Older Windows has no presets, and then the feature is not advertised.

// preset is one of Windows' connection parameter presets with its values, in HCI units.
type preset struct {
	name string
	obj  *bluetooth.BluetoothLEPreferredConnectionParameters
	ble.ConnParams
}

// loadPresets reads the presets, or returns nil before Windows 11.
func loadPresets() ([]preset, error) {
	gets := []struct {
		name string
		get  func() (*bluetooth.BluetoothLEPreferredConnectionParameters, error)
	}{
		{"throughput", bluetooth.BluetoothLEPreferredConnectionParametersGetThroughputOptimized},
		{"balanced", bluetooth.BluetoothLEPreferredConnectionParametersGetBalanced},
		{"power", bluetooth.BluetoothLEPreferredConnectionParametersGetPowerOptimized},
	}
	var out []preset
	for _, g := range gets {
		o, err := g.get()
		if err != nil || o == nil {
			for _, p := range out {
				p.obj.Release()
			}
			return nil, fmt.Errorf("preset %s: %v", g.name, err)
		}
		p := preset{name: g.name, obj: o}
		p.MinInterval, _ = o.GetMinConnectionInterval()
		p.MaxInterval, _ = o.GetMaxConnectionInterval()
		p.Latency, _ = o.GetConnectionLatency()
		p.Timeout, _ = o.GetLinkTimeout()
		out = append(out, p)
	}
	return out, nil
}

// choosePreset picks the preset whose interval range is nearest to the requested one, on a log scale: the
// presets are about factors apart (7.5 ms, 30 ms, 90 ms and up). Ranges that overlap the request win.
func choosePreset(req ble.ConnParams, presets []preset) int {
	mid := func(lo, hi uint16) float64 { return math.Sqrt(float64(max(lo, 1)) * float64(max(hi, lo, 1))) }
	want := mid(req.MinInterval, req.MaxInterval)
	best, bestD := 0, math.Inf(1)
	for i, p := range presets {
		d := math.Abs(math.Log(mid(p.MinInterval, p.MaxInterval) / want))
		if req.MinInterval <= p.MaxInterval && p.MinInterval <= req.MaxInterval {
			d -= 100 // overlapping
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// ConnParamsSupported reports whether this Windows has connection parameter presets.
func (a *Adapter) ConnParamsSupported() bool { return len(a.presets) > 0 }

// SetConnParams asks Windows for the preset nearest to p on the connection to addr.
func (a *Adapter) SetConnParams(addr uint64, p ble.ConnParams) error {
	if len(a.presets) == 0 {
		return errors.New("connection parameters need Windows 11")
	}
	a.mu.Lock()
	c := a.conns[addr]
	a.mu.Unlock()
	if c == nil {
		return ble.ErrNotConnected
	}
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	c.mu.Lock()
	up := c.up
	c.mu.Unlock()
	if !up || c.dev == nil {
		return ble.ErrNotConnected
	}
	pr := a.presets[choosePreset(p, a.presets)]
	req, err := c.dev.RequestPreferredConnectionParameters(pr.obj)
	if err != nil {
		return err
	}
	st, err := req.GetStatus()
	if err != nil || st != bluetooth.BluetoothLEPreferredConnectionParametersRequestStatusSuccess {
		req.Close()
		req.Release()
		if err != nil {
			return err
		}
		return fmt.Errorf("preset %s refused with status %d", pr.name, st)
	}
	c.mu.Lock()
	old := c.cpReq
	c.cpReq = req
	c.mu.Unlock()
	if old != nil {
		old.Close()
		old.Release()
	}
	log := a.log.With("addr", ble.FormatMAC(addr), "preset", pr.name, "min", pr.MinInterval, "max", pr.MaxInterval,
		"latency", pr.Latency, "timeout", pr.Timeout)
	if cur, err := c.dev.GetConnectionParameters(); err == nil && cur != nil {
		iv, _ := cur.GetConnectionInterval()
		lat, _ := cur.GetConnectionLatency()
		to, _ := cur.GetLinkTimeout()
		cur.Release()
		log = log.With("current_interval", iv, "current_latency", lat, "current_timeout", to)
	}
	log.Debug("connection parameter preset requested")
	return nil
}

var (
	_ ble.ConnParamsSetter  = (*Adapter)(nil)
	_ ble.ConnParamsSupport = (*Adapter)(nil)
)
