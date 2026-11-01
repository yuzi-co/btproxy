package ble

import "testing"

type plainAdapter struct{ Adapter }

type setterAdapter struct{ Adapter }

func (setterAdapter) SetConnParams(uint64, ConnParams) error { return nil }

type runtimeAdapter struct {
	setterAdapter
	ok bool
}

func (r runtimeAdapter) ConnParamsSupported() bool { return r.ok }

func TestCanSetConnParams(t *testing.T) {
	for _, c := range []struct {
		a    Adapter
		want bool
	}{
		{plainAdapter{}, false},
		{setterAdapter{}, true},
		{runtimeAdapter{ok: true}, true},
		{runtimeAdapter{ok: false}, false},
	} {
		if _, ok := CanSetConnParams(c.a); ok != c.want {
			t.Errorf("%T %+v: %v", c.a, c.a, ok)
		}
	}
}
