package kernel

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// bondStore keeps the keys of bonded devices in one JSON file per controller, since /var/lib/bluetooth belongs
// to BlueZ. Keys are kept as the bytes the kernel reports and loads, in hex. Entries are per key: a device can
// have one long term key as central and one as peripheral.
type bondStore struct {
	mu   sync.Mutex
	path string
	data bondFile
}

type bondFile struct {
	LTKs []string `json:"ltks"`
	IRKs []string `json:"irks"`
}

func openBonds(dir string, mac uint64) (*bondStore, error) {
	s := &bondStore{path: filepath.Join(dir, fmt.Sprintf("bonds-%012x.json", mac))}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return s, nil
}

func (s *bondStore) save() error {
	b, err := json.MarshalIndent(s.data, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".bonds-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

// sameKey compares the part of a stored key that identifies it: for an LTK the address, its type and the role,
// for an IRK the address and its type.
func sameKey(a, b []byte, idLen int, role int) bool {
	if len(a) < idLen || len(b) < idLen || string(a[:idLen]) != string(b[:idLen]) {
		return false
	}
	return role < 0 || (len(a) > role && len(b) > role && a[role] == b[role])
}

func put(list []string, key []byte, role int) []string {
	list = slices.DeleteFunc(list, func(e string) bool {
		old, err := hex.DecodeString(e)
		return err != nil || sameKey(old, key, 7, role)
	})
	return append(list, hex.EncodeToString(key))
}

func (s *bondStore) putLTK(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LTKs = put(s.data.LTKs, key, 8) // byte 8 is the central flag
	return s.save()
}

func (s *bondStore) putIRK(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.IRKs = put(s.data.IRKs, key, -1)
	return s.save()
}

func decode(list []string, size int) [][]byte {
	var keys [][]byte
	for _, e := range list {
		if b, err := hex.DecodeString(e); err == nil && len(b) == size {
			keys = append(keys, b)
		}
	}
	return keys
}

// has tells whether there is a long term key for the device.
func (s *bondStore) has(addr uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range decode(s.data.LTKs, ltkSize) {
		if getAddr(k) == addr {
			return true
		}
	}
	return false
}

// forget removes every key of the device and returns the address types it was bonded under.
func (s *bondStore) forget(addr uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var types []byte
	drop := func(e string) bool {
		b, err := hex.DecodeString(e)
		if err != nil || len(b) < 7 || getAddr(b) != addr {
			return false
		}
		if !slices.Contains(types, b[6]) {
			types = append(types, b[6])
		}
		return true
	}
	l, i := len(s.data.LTKs), len(s.data.IRKs)
	s.data.LTKs = slices.DeleteFunc(s.data.LTKs, drop)
	s.data.IRKs = slices.DeleteFunc(s.data.IRKs, drop)
	if l == len(s.data.LTKs) && i == len(s.data.IRKs) {
		return nil, nil
	}
	return types, s.save()
}

// loadParam builds the parameter of Load Long Term Keys or Load Identity Resolving Keys: a count, then the keys.
func loadParam(keys [][]byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, uint16(len(keys)))
	for _, k := range keys {
		b = append(b, k...)
	}
	return b
}

func (s *bondStore) ltkParam() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadParam(decode(s.data.LTKs, ltkSize))
}

func (s *bondStore) irkParam() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return loadParam(decode(s.data.IRKs, irkSize))
}

// describe names a bond in a log line without showing its key.
func describe(b []byte) string { return ble.FormatMAC(getAddr(b)) }
