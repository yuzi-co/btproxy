package ble

import (
	"slices"
	"testing"
)

// TestScanCellModes: the effective mode follows the requested one until passive scanning is unavailable.
func TestScanCellModes(t *testing.T) {
	var c ScanCell
	if c.Mode() != ScanPassive || c.Effective() != ScanPassive || c.Setting() != "Passive" {
		t.Fatalf("zero cell: mode %v effective %v setting %q", c.Mode(), c.Effective(), c.Setting())
	}
	c.SetMode(ScanActive)
	if c.Effective() != ScanActive || c.Setting() != "Active" {
		t.Fatalf("active: effective %v setting %q", c.Effective(), c.Setting())
	}
	c.SetMode(ScanPassive)
	c.SetActiveOnly(true)
	if !c.ActiveOnly() || c.Effective() != ScanActive || c.Setting() != "Active, passive unavailable" {
		t.Fatalf("fallback: effective %v setting %q", c.Effective(), c.Setting())
	}
	c.SetActiveOnly(false)
	if c.Effective() != ScanPassive {
		t.Fatalf("after the fallback: effective %v", c.Effective())
	}
}

// TestScanCellNotifiesChanges: the callback gets each change once, with the effective mode, and nothing when
// the status is set to what it is.
func TestScanCellNotifiesChanges(t *testing.T) {
	var c ScanCell
	var got []ScanStatus
	c.OnStatus(func(st ScanStatus) {
		got = append(got, st)
		if c.Status() != st { // the cell may be read from the callback
			t.Errorf("status %+v during the callback for %+v", c.Status(), st)
		}
	})
	c.SetState(ScanStarting)
	c.SetState(ScanStarting)
	c.SetActiveOnly(true)
	c.SetState(ScanRunning)
	c.Set(ScanStatus{State: ScanRunning, Mode: ScanPassive})
	want := []ScanStatus{{ScanStarting, ScanPassive}, {ScanRunning, ScanActive}, {ScanRunning, ScanPassive}}
	if !slices.Equal(got, want) {
		t.Errorf("callback got %+v, want %+v", got, want)
	}
	if c.Status() != want[2] {
		t.Errorf("status %+v, want %+v", c.Status(), want[2])
	}
}

// TestScanCellPublish: Store keeps the callback out until Publish, which reports the latest status once.
func TestScanCellPublish(t *testing.T) {
	var c ScanCell
	var got []ScanStatus
	c.OnStatus(func(st ScanStatus) { got = append(got, st) })
	c.StoreState(ScanStarting)
	c.Store(ScanStatus{State: ScanRunning, Mode: ScanActive})
	if len(got) != 0 {
		t.Fatalf("callback called by Store: %+v", got)
	}
	c.Publish()
	c.Publish()
	want := []ScanStatus{{ScanRunning, ScanActive}}
	if !slices.Equal(got, want) {
		t.Errorf("callback got %+v, want %+v", got, want)
	}
}

// TestScanCellWithoutCallback: setting the status before OnStatus or without a callback is fine.
func TestScanCellWithoutCallback(t *testing.T) {
	var c ScanCell
	c.SetState(ScanRunning)
	if c.Status() != (ScanStatus{State: ScanRunning}) {
		t.Errorf("status %+v", c.Status())
	}
}
