package api

import (
	"bytes"

	"github.com/yuzi-co/btproxy/internal/ble"

	"google.golang.org/protobuf/encoding/protowire"
)

// Message type ids from ESPHome's api.proto.
const (
	msgHelloRequest                     = 1
	msgHelloResponse                    = 2
	msgDisconnectRequest                = 5
	msgDisconnectResponse               = 6
	msgPingRequest                      = 7
	msgPingResponse                     = 8
	msgDeviceInfoRequest                = 9
	msgDeviceInfoResponse               = 10
	msgListEntitiesRequest              = 11
	msgListEntitiesBinarySensorResponse = 12
	msgListEntitiesSensorResponse       = 16
	msgListEntitiesSwitchResponse       = 17
	msgListEntitiesTextSensorResponse   = 18
	msgListEntitiesDoneResponse         = 19
	msgSubscribeStatesRequest           = 20
	msgBinarySensorStateResponse        = 21
	msgSensorStateResponse              = 25
	msgSwitchStateResponse              = 26
	msgTextSensorStateResponse          = 27
	msgSubscribeLogsRequest             = 28
	msgSubscribeLogsResponse            = 29
	msgSwitchCommandRequest             = 33
	msgSubscribeHAServicesRequest       = 34
	msgGetTimeResponse                  = 37
	msgSubscribeHAStatesRequest         = 38
	msgListEntitiesButtonResponse       = 61
	msgButtonCommandRequest             = 62
	msgSubscribeBTAdvertisements        = 66
	msgBTDeviceRequest                  = 68
	msgBTDeviceConnectionResponse       = 69
	msgBTGATTGetServicesRequest         = 70
	msgBTGATTGetServicesResponse        = 71
	msgBTGATTGetServicesDoneResponse    = 72
	msgBTGATTReadRequest                = 73
	msgBTGATTReadResponse               = 74
	msgBTGATTWriteRequest               = 75
	msgBTGATTReadDescriptorRequest      = 76
	msgBTGATTWriteDescriptorRequest     = 77
	msgBTGATTNotifyRequest              = 78
	msgBTGATTNotifyDataResponse         = 79
	msgSubscribeBTConnectionsFree       = 80
	msgBTConnectionsFreeResponse        = 81
	msgBTGATTErrorResponse              = 82
	msgBTGATTWriteResponse              = 83
	msgBTGATTNotifyResponse             = 84
	msgBTDevicePairingResponse          = 85
	msgBTDeviceUnpairingResponse        = 86
	msgUnsubscribeBTAdvertisements      = 87
	msgBTDeviceClearCacheResponse       = 88
	msgBTRawAdvertisementsResponse      = 93
	msgNoiseEncryptionSetKeyRequest     = 124
	msgNoiseEncryptionSetKeyResponse    = 125
	msgBTScannerStateResponse           = 126
	msgBTScannerSetModeRequest          = 127
	msgBTSetConnParamsRequest           = 145
	msgBTSetConnParamsResponse          = 146
	msgDeviceCapabilitiesRequest        = 149
	msgDeviceCapabilitiesResponse       = 150
)

// API version this server speaks (ESPHome 2026.x).
const apiMajor, apiMinor = 1, 19

// Bluetooth proxy feature flags (DeviceInfoResponse.bluetooth_proxy_feature_flags).
const (
	featurePassiveScan       = 1 << 0
	featureActiveConnections = 1 << 1
	featureRemoteCaching     = 1 << 2
	featurePairing           = 1 << 3
	featureCacheClearing     = 1 << 4
	featureRawAdvertisements = 1 << 5
	featureStateAndMode      = 1 << 6
	featureConnParams        = 1 << 7
)

// Bluetooth device request types.
const (
	reqDisconnect            = 1
	reqPair                  = 2
	reqUnpair                = 3
	reqConnectV3WithCache    = 4
	reqConnectV3WithoutCache = 5
	reqClearCache            = 6
)

// gattError is reported for failed GATT operations and connections (ESP_GATT_ERROR, 0x85). aioesphomeapi
// lists it in its BLE error table and Home Assistant shows "GATT operation error" for it.
const gattError = 0x85

// gattNotConnected is ESPHome's own code for a pairing or connection parameter request on a device that is
// not connected: GATT_NOT_CONNECTED in its bluetooth_proxy component, -1 on purpose, since no ESP-IDF GATT
// status is negative. Unlike ESPHome, a GATT request on a device that is not connected gets gattError here;
// the client fails the request on either code.
const gattNotConnected = -1

type deviceInfo struct {
	Name, FriendlyName, MAC, BTMAC string
	Version, Model, Manufacturer   string
	ProjectName, ProjectVersion    string
	SuggestedArea                  string
	FeatureFlags                   uint32
	EncryptionSupported            bool
}

func encHelloResponse(serverInfo, name string) []byte {
	var e enc
	e.uint(1, apiMajor)
	e.uint(2, apiMinor)
	e.str(3, serverInfo)
	e.str(4, name)
	return e.b
}

func encDeviceInfo(d deviceInfo) []byte {
	var e enc
	e.str(2, d.Name)
	e.str(3, d.MAC)
	e.str(4, d.Version)
	e.str(6, d.Model)
	e.str(8, d.ProjectName)
	e.str(9, d.ProjectVersion)
	e.str(12, d.Manufacturer)
	e.str(13, d.FriendlyName)
	e.uint(15, uint64(d.FeatureFlags))
	e.str(16, d.SuggestedArea)
	e.str(18, d.BTMAC)
	// Home Assistant offers to provision a key on a plaintext device that claims encryption support, which the
	// proxy cannot store, so it is reported only while the API is encrypted.
	e.bool(19, d.EncryptionSupported)
	return e.b
}

// encCapabilities answers DeviceCapabilitiesRequest, which Home Assistant sends after device info.
func encCapabilities(flags uint32, btMAC string) []byte {
	var bt enc
	bt.uint(1, uint64(flags))
	bt.str(2, btMAC)
	var e enc
	e.msg(1, bt.b)
	return e.b
}

// appendRawAdvert appends one advertisement to a BluetoothLERawAdvertisementsResponse being built in b. The
// entry's size is worked out first, so it is encoded in one pass, in place.
func appendRawAdvert(b []byte, a ble.Advert) []byte {
	rssi := protowire.EncodeZigZag(int64(a.RSSI))
	n := 0
	if a.Addr != 0 {
		n += protowire.SizeTag(1) + protowire.SizeVarint(a.Addr)
	}
	if rssi != 0 {
		n += protowire.SizeTag(2) + protowire.SizeVarint(rssi)
	}
	if a.AddrType != 0 {
		n += protowire.SizeTag(3) + protowire.SizeVarint(uint64(a.AddrType))
	}
	if len(a.Data) > 0 {
		n += protowire.SizeTag(4) + protowire.SizeBytes(len(a.Data))
	}
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	e := enc{protowire.AppendVarint(b, uint64(n))}
	e.uint(1, a.Addr)
	e.sint(2, int64(a.RSSI))
	e.uint(3, uint64(a.AddrType))
	e.bytes(4, a.Data)
	return e.b
}

func encConnection(addr uint64, connected bool, mtu uint32, errCode int32) []byte {
	var e enc
	e.uint(1, addr)
	e.bool(2, connected)
	e.uint(3, uint64(mtu))
	e.int32(4, errCode)
	return e.b
}

func encConnectionsFree(free, limit uint32, allocated []uint64) []byte {
	var e enc
	e.uint(1, uint64(free))
	e.uint(2, uint64(limit))
	for _, a := range allocated {
		e.uint(3, a)
	}
	return e.b
}

// efficientUUIDs reports whether a client of the given API version reads short_uuid (API 1.12 and later).
func efficientUUIDs(major, minor uint32) bool { return major > 1 || major == 1 && minor >= 12 }

// encUUID writes one form of a UUID, as ESPHome does: short_uuid when the UUID is on the Bluetooth base and
// the client reads it, otherwise the 128-bit form (both halves, even zero ones: their position matters).
func encUUID(e *enc, u ble.UUID, shortField protowire.Number, efficient bool) {
	if s, ok := u.Short(); ok && efficient && s != 0 {
		e.uint(shortField, uint64(s))
		return
	}
	for _, v := range []uint64{u.Hi, u.Lo} {
		e.b = protowire.AppendTag(e.b, 1, protowire.VarintType)
		e.b = protowire.AppendVarint(e.b, v)
	}
}

// encServices encodes one GetServicesResponse holding the given services.
func encServices(addr uint64, services []ble.Service, efficient bool) []byte {
	var e enc
	e.uint(1, addr)
	for _, s := range services {
		var se enc
		encUUID(&se, s.UUID, 4, efficient)
		se.uint(2, uint64(s.Handle))
		for _, c := range s.Characteristics {
			var ce enc
			encUUID(&ce, c.UUID, 5, efficient)
			ce.uint(2, uint64(c.Handle))
			ce.uint(3, uint64(c.Properties))
			for _, d := range c.Descriptors {
				var de enc
				encUUID(&de, d.UUID, 3, efficient)
				de.uint(2, uint64(d.Handle))
				ce.msg(4, de.b)
			}
			se.msg(3, ce.b)
		}
		e.msg(2, se.b)
	}
	return e.b
}

// encAddrHandle covers the responses that carry only an address and a handle (write, notify).
func encAddrHandle(addr uint64, handle uint16) []byte {
	var e enc
	e.uint(1, addr)
	e.uint(2, uint64(handle))
	return e.b
}

// encAddrHandleData covers read results and notification data.
func encAddrHandleData(addr uint64, handle uint16, data []byte) []byte {
	var e enc
	e.uint(1, addr)
	e.uint(2, uint64(handle))
	e.bytes(3, data)
	return e.b
}

func encGATTError(addr uint64, handle uint16, code int32) []byte {
	var e enc
	e.uint(1, addr)
	e.uint(2, uint64(handle))
	e.int32(3, code)
	return e.b
}

// encAddrResult covers the pairing, unpairing and cache-clearing responses.
func encAddrResult(addr uint64, ok bool, code int32) []byte {
	var e enc
	e.uint(1, addr)
	e.bool(2, ok)
	e.int32(3, code)
	return e.b
}

func encConnParamsResult(addr uint64, code int32) []byte {
	var e enc
	e.uint(1, addr)
	e.int32(2, code)
	return e.b
}

func encScannerState(state ble.ScanState, mode, configured ble.ScanMode) []byte {
	var e enc
	e.uint(1, uint64(state))
	e.uint(2, uint64(mode))
	e.uint(3, uint64(configured))
	return e.b
}

// decHello returns the client's API version from a HelloRequest.
func decHello(b []byte) (major, minor uint32, err error) {
	fs, err := fields(b)
	for _, f := range fs {
		switch f.num {
		case 2:
			major = uint32(f.v)
		case 3:
			minor = uint32(f.v)
		}
	}
	return major, minor, err
}

// decSubscribeLogs returns the level and the dump_config flag of a SubscribeLogsRequest.
func decSubscribeLogs(b []byte) (level int32, dump bool, err error) {
	fs, err := fields(b)
	for _, f := range fs {
		switch f.num {
		case 1:
			level = int32(min(f.v, logVeryVerbose))
		case 2:
			dump = f.v != 0
		}
	}
	return level, dump, err
}

// decConnParams decodes a BluetoothSetConnectionParamsRequest. The fields are uint32 on the wire but 16 bits
// in Bluetooth; larger values are clamped, as ESPHome does, and left for the controller to refuse.
func decConnParams(b []byte) (addr uint64, p ble.ConnParams, err error) {
	fs, err := fields(b)
	clamp := func(v uint64) uint16 { return uint16(min(v, 0xffff)) }
	for _, f := range fs {
		switch f.num {
		case 1:
			addr = f.v
		case 2:
			p.MinInterval = clamp(f.v)
		case 3:
			p.MaxInterval = clamp(f.v)
		case 4:
			p.Latency = clamp(f.v)
		case 5:
			p.Timeout = clamp(f.v)
		}
	}
	return addr, p, err
}

// request holds the fields shared by the Bluetooth requests: address, handle, request type, flags and data.
type request struct {
	addr     uint64
	handle   uint16
	kind     uint32 // request_type for device requests, flags for subscriptions, scanner mode
	addrType uint32
	flag     bool // response for writes, enable for notify
	data     []byte
}

// reqField says which request field a protobuf field of a Bluetooth request sets.
type reqField uint8

const (
	fieldNone reqField = iota
	fieldAddr
	fieldHandle
	fieldKind
	fieldAddrType
	fieldFlag
	fieldData
)

// reqLayouts gives the fields of each Bluetooth request the proxy reads, by message type and field number:
// the numbers api.proto declares for the message, which the proto test checks.
var reqLayouts = map[uint32][]reqField{
	msgBTDeviceRequest:              {1: fieldAddr, 2: fieldKind, 4: fieldAddrType},
	msgSubscribeBTAdvertisements:    {1: fieldKind}, // flags
	msgBTScannerSetModeRequest:      {1: fieldKind}, // mode
	msgBTGATTGetServicesRequest:     {1: fieldAddr},
	msgBTGATTReadRequest:            {1: fieldAddr, 2: fieldHandle},
	msgBTGATTWriteRequest:           {1: fieldAddr, 2: fieldHandle, 3: fieldFlag, 4: fieldData},
	msgBTGATTReadDescriptorRequest:  {1: fieldAddr, 2: fieldHandle},
	msgBTGATTWriteDescriptorRequest: {1: fieldAddr, 2: fieldHandle, 3: fieldData},
	msgBTGATTNotifyRequest:          {1: fieldAddr, 2: fieldHandle, 3: fieldFlag},
}

// decRequest decodes a Bluetooth request by its layout in reqLayouts. Write data is copied out of the frame:
// a request waits in its connection's queue, where only the data counts toward the byte limit, and the frame
// must not be held with it.
func decRequest(typ uint32, b []byte) (request, error) {
	fs, err := fields(b)
	if err != nil {
		return request{}, err
	}
	layout := reqLayouts[typ]
	var r request
	for _, f := range fs {
		if int(f.num) >= len(layout) {
			continue
		}
		switch layout[f.num] {
		case fieldAddr:
			r.addr = f.v
		case fieldHandle:
			r.handle = uint16(f.v)
		case fieldKind:
			r.kind = uint32(f.v)
		case fieldAddrType:
			r.addrType = uint32(f.v)
		case fieldFlag:
			r.flag = f.v != 0
		case fieldData:
			r.data = bytes.Clone(f.data)
		}
	}
	return r, nil
}
