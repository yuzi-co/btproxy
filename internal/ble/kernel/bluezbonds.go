package kernel

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const keyLen = 16

// copyBlueZ fills a store that does not exist yet from BlueZ's files, so that moving to this backend does not mean
// pairing every device again. It runs once per controller: later, those files say nothing about what this backend
// has paired or unpaired. BlueZ's files are only read. A failure costs a pairing, not the start.
func (s *bondStore) copyBlueZ(dir string, mac uint64, log *logx.Logger) {
	if _, err := os.Stat(s.path); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	ctrl := filepath.Join(dir, ble.FormatMAC(mac))
	entries, err := os.ReadDir(ctrl)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("cannot read BlueZ's bonds", "err", err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var copied []string
	for _, e := range entries {
		peer, err := ble.ParseMAC(e.Name())
		if err != nil { // BlueZ also keeps its cache and settings here
			continue
		}
		b, err := os.ReadFile(filepath.Join(ctrl, e.Name(), "info"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Warn("cannot read a BlueZ bond", "device", e.Name(), "err", err)
			continue
		}
		ltks, irk, err := parseBlueZInfo(peer, string(b))
		if err != nil {
			log.Warn("cannot use a BlueZ bond, the device must pair again", "device", e.Name(), "err", err)
			continue
		}
		if len(ltks) == 0 { // not bonded, or over classic Bluetooth, which this backend does not use
			continue
		}
		for _, k := range ltks {
			s.data.LTKs = put(s.data.LTKs, k, 8)
		}
		if irk != nil {
			s.data.IRKs = put(s.data.IRKs, irk, -1)
		}
		copied = append(copied, e.Name())
	}
	if len(copied) == 0 {
		return
	}
	if err := s.save(); err != nil {
		log.Warn("cannot save the bonds copied from BlueZ, they last until the next start", "err", err)
	}
	log.Info("copied bonds from BlueZ", "devices", copied)
}

// parseBlueZInfo reads the long term keys and the identity key from a device's info file, as the kernel's Load
// commands take them. Authenticated holds the kernel's own key type: BlueZ stores it unchanged.
func parseBlueZInfo(peer uint64, text string) (ltks [][]byte, irk []byte, err error) {
	groups := parseINI(text)
	addrType := byte(bdAddrLEPublic)
	if groups["General"]["AddressType"] == "static" {
		addrType = bdAddrLERandom
	}
	// Central keys, then peripheral ones. PeripheralLongTermKey replaced SlaveLongTermKey and BlueZ writes both.
	if g, ok := groups["LongTermKey"]; ok {
		central := g["Master"] != "false"
		if ltks, err = appendLTK(ltks, peer, addrType, central, g); err != nil {
			return nil, nil, err
		}
	}
	g, ok := groups["PeripheralLongTermKey"]
	if !ok {
		g, ok = groups["SlaveLongTermKey"]
	}
	if ok {
		if ltks, err = appendLTK(ltks, peer, addrType, false, g); err != nil {
			return nil, nil, err
		}
	}
	if g, ok := groups["IdentityResolvingKey"]; ok {
		val, err := parseKey(g["Key"])
		if err != nil {
			return nil, nil, fmt.Errorf("identity key: %w", err)
		}
		irk = append(addrInfo(peer, addrType), val...)
	}
	return ltks, irk, nil
}

func appendLTK(ltks [][]byte, peer uint64, addrType byte, central bool, g map[string]string) ([][]byte, error) {
	val, err := parseKey(g["Key"])
	if err != nil {
		return nil, fmt.Errorf("long term key: %w", err)
	}
	keyType, err1 := strconv.ParseUint(g["Authenticated"], 10, 8)
	encSize, err2 := strconv.ParseUint(g["EncSize"], 10, 8)
	ediv, err3 := strconv.ParseUint(g["EDiv"], 10, 16)
	rnd, err4 := strconv.ParseUint(g["Rand"], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, fmt.Errorf("long term key: %w", err)
	}
	k := addrInfo(peer, addrType)
	k = append(k, byte(keyType), 0, byte(encSize))
	if central {
		k[8] = 1
	}
	k = binary.LittleEndian.AppendUint16(k, uint16(ediv))
	k = binary.LittleEndian.AppendUint64(k, rnd)
	return append(ltks, append(k, val...)), nil
}

// parseKey decodes a key. Errors do not quote it.
func parseKey(s string) ([]byte, error) {
	val, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(val) != keyLen {
		return nil, errors.New("not 16 bytes of hex")
	}
	return val, nil
}

// parseINI reads the small part of the key file format that BlueZ's info files use.
func parseINI(text string) map[string]map[string]string {
	groups := map[string]map[string]string{}
	var cur map[string]string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "["); ok {
			if name, ok = strings.CutSuffix(name, "]"); ok {
				cur = map[string]string{}
				groups[name] = cur
			}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != nil && !strings.HasPrefix(line, "#") {
			cur[k] = v
		}
	}
	return groups
}
