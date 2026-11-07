package winrt

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// In active mode Windows reports an empty scan response as an entry of its own, which tells Home Assistant
// nothing: the device's advertisements arrive anyway.
func TestDeliverDropsEmptyAdverts(t *testing.T) {
	a := testAdapter(t)
	var got []ble.Advert
	fn := func(ad ble.Advert) { got = append(got, ad) }
	a.fn.Store(&fn)

	a.deliver(ble.Advert{Addr: 1})
	a.deliver(ble.Advert{Addr: 2, Data: []byte{}})
	if len(got) != 0 {
		t.Fatalf("empty adverts delivered: %+v", got)
	}
	a.deliver(ble.Advert{Addr: 3, Data: []byte{2, 1, 6}})
	if len(got) != 1 || got[0].Addr != 3 {
		t.Fatalf("delivered %+v", got)
	}
}

func TestDeliverWithoutAScan(t *testing.T) {
	testAdapter(t).deliver(ble.Advert{Addr: 1, Data: []byte{2, 1, 6}})
}
