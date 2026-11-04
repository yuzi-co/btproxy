package kernel

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

const (
	testCentralKey    = "00112233445566778899AABBCCDDEEFF"
	testPeripheralKey = "FFEEDDCCBBAA99887766554433221100"
	testIRK           = "0F1E2D3C4B5A69788796A5B4C3D2E1F0"
)

// writeBlueZ lays out a device as BlueZ keeps it, under the controller the fake kernel reports.
func writeBlueZ(t *testing.T, dir string, peer uint64, info string) {
	t.Helper()
	if isWindows() {
		t.Skip("BlueZ's directory names have colons, which Windows does not allow")
	}
	devDir := filepath.Join(dir, ble.FormatMAC(testMAC), ble.FormatMAC(peer))
	if err := os.MkdirAll(devDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "info"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
}

// wantLTK builds struct mgmt_ltk_info by hand, so that the test does not share a mistake with the code.
func wantLTK(peer uint64, addrType, keyType, central, encSize byte, ediv uint16, rnd uint64, key string) []byte {
	b := addrInfo(peer, addrType)
	b = append(b, keyType, central, encSize)
	b = binary.LittleEndian.AppendUint16(b, ediv)
	b = binary.LittleEndian.AppendUint64(b, rnd)
	val, err := hex.DecodeString(key)
	if err != nil {
		panic(err)
	}
	return append(b, val...)
}

func wantIRK(peer uint64, addrType byte, key string) []byte {
	val, err := hex.DecodeString(key)
	if err != nil {
		panic(err)
	}
	return append(addrInfo(peer, addrType), val...)
}

func loadOf(keys ...[]byte) []byte { return loadParam(keys) }

const staticPeer = 0xF424A206A947

func TestBondsCopiedFromBlueZ(t *testing.T) {
	blueZ := t.TempDir()
	writeBlueZ(t, blueZ, staticPeer, `[General]
Name=Breezer 4S
AddressType=static
SupportedTechnologies=LE;

[IdentityResolvingKey]
Key=`+testIRK+`

[LongTermKey]
Key=`+testCentralKey+`
Authenticated=0
EncSize=16
EDiv=4660
Rand=72623859790382856

[PeripheralLongTermKey]
Key=`+testPeripheralKey+`
Authenticated=1
EncSize=7
EDiv=1
Rand=2

[SlaveLongTermKey]
Key=00000000000000000000000000000000
Authenticated=0
EncSize=16
EDiv=0
Rand=0
`)
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.BlueZDir = blueZ })

	want := loadOf(
		wantLTK(staticPeer, bdAddrLERandom, 0, 1, 16, 4660, 72623859790382856, testCentralKey),
		wantLTK(staticPeer, bdAddrLERandom, 1, 0, 7, 1, 2, testPeripheralKey))
	if got := k.lastParam(opLoadLTKs); !bytes.Equal(got, want) {
		t.Errorf("LTKs\n got % x\nwant % x", got, want)
	}
	if got, want := k.lastParam(opLoadIRKs), loadOf(wantIRK(staticPeer, bdAddrLERandom, testIRK)); !bytes.Equal(got, want) {
		t.Errorf("IRKs\n got % x\nwant % x", got, want)
	}
	if !a.bonds.has(staticPeer) {
		t.Error("device not bonded after the copy")
	}
}

func TestUnpairedDeviceNotCopiedAgain(t *testing.T) {
	blueZ := t.TempDir()
	writeBlueZ(t, blueZ, staticPeer, "[General]\nAddressType=static\n\n[LongTermKey]\nKey="+testCentralKey+
		"\nAuthenticated=0\nEncSize=16\nEDiv=0\nRand=0\n")
	dir := t.TempDir()
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.BondDir = dir; o.BlueZDir = blueZ })
	if err := a.Unpair(staticPeer); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// The copy happens once: unpairing must not be undone at the next start by BlueZ's files, which stay.
	k2 := newFakeKernel()
	openTest(t, k2, func(o *Options) { o.BondDir = dir; o.BlueZDir = blueZ })
	if got := k2.lastParam(opLoadLTKs); !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("unpaired device came back: % x", got)
	}
}

func TestBlueZCopySkipsWhatItCannotUse(t *testing.T) {
	blueZ := t.TempDir()
	good := uint64(0xC00000000001)
	writeBlueZ(t, blueZ, good, "[General]\nAddressType=public\n\n[LongTermKey]\nKey="+testCentralKey+
		"\nAuthenticated=0\nEncSize=16\nEDiv=0\nRand=0\n")
	writeBlueZ(t, blueZ, 0xC00000000002, "[General]\nAddressType=public\n\n[LongTermKey]\nKey=short\n"+
		"Authenticated=0\nEncSize=16\nEDiv=0\nRand=0\n")
	writeBlueZ(t, blueZ, 0xC00000000003, "[General]\nSupportedTechnologies=BR/EDR;\n\n[LinkKey]\nKey="+testCentralKey+"\n")
	// BlueZ's other entries under the controller are not devices.
	if err := os.MkdirAll(filepath.Join(blueZ, ble.FormatMAC(testMAC), "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blueZ, ble.FormatMAC(testMAC), "settings"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	k := newFakeKernel()
	openTest(t, k, func(o *Options) { o.BlueZDir = blueZ })
	want := loadOf(wantLTK(good, bdAddrLEPublic, 0, 1, 16, 0, 0, testCentralKey))
	if got := k.lastParam(opLoadLTKs); !bytes.Equal(got, want) {
		t.Errorf("LTKs\n got % x\nwant % x", got, want)
	}
}

// Old BlueZ versions kept both roles' keys in [LongTermKey] and told them apart with Master.
func TestBlueZCopyReadsOldMasterField(t *testing.T) {
	blueZ := t.TempDir()
	writeBlueZ(t, blueZ, staticPeer, "[General]\nAddressType=static\n\n[LongTermKey]\nKey=0x"+testCentralKey+
		"\nAuthenticated=0\nEncSize=16\nEDiv=0\nRand=0\nMaster=false\n")
	k := newFakeKernel()
	openTest(t, k, func(o *Options) { o.BlueZDir = blueZ })
	want := loadOf(wantLTK(staticPeer, bdAddrLERandom, 0, 0, 16, 0, 0, testCentralKey))
	if got := k.lastParam(opLoadLTKs); !bytes.Equal(got, want) {
		t.Errorf("LTKs\n got % x\nwant % x", got, want)
	}
}

func TestNoBlueZDirectoryIsFine(t *testing.T) {
	k := newFakeKernel()
	dir := t.TempDir()
	openTest(t, k, func(o *Options) { o.BondDir = dir; o.BlueZDir = filepath.Join(dir, "missing") })
	if got := k.lastParam(opLoadLTKs); !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("LTKs % x", got)
	}
}
