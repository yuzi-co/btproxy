package api

import (
	"btproxy/internal/ble"

	"google.golang.org/protobuf/encoding/protowire"
)

// Message type ids from ESPHome's api.proto.
const (
	msgHelloRequest                  = 1
	msgHelloResponse                 = 2
	msgDisconnectRequest             = 5
	msgDisconnectResponse            = 6
	msgPingRequest                   = 7
	msgPingResponse                  = 8
	msgDeviceInfoRequest             = 9
	msgDeviceInfoResponse            = 10
	msgListEntitiesRequest           = 11
	msgListEntitiesDoneResponse      = 19
	msgSubscribeStatesRequest        = 20
	msgSubscribeLogsRequest          = 28
	msgSubscribeHAServicesRequest    = 34
	msgGetTimeResponse               = 37
	msgSubscribeHAStatesRequest      = 38
	msgSubscribeBTAdvertisements     = 66
	msgBTDeviceRequest               = 68
	msgBTDeviceConnectionResponse    = 69
	msgBTGATTGetServicesRequest      = 70
	msgBTGATTGetServicesResponse     = 71
	msgBTGATTGetServicesDoneResponse = 72
	msgBTGATTReadRequest             = 73
	msgBTGATTReadResponse            = 74
	msgBTGATTWriteRequest            = 75
	msgBTGATTReadDescriptorRequest   = 76
	msgBTGATTWriteDescriptorRequest  = 77
	msgBTGATTNotifyRequest           = 78
	msgBTGATTNotifyDataResponse      = 79
	msgSubscribeBTConnectionsFree    = 80
	msgBTConnectionsFreeResponse     = 81
	msgBTGATTErrorResponse           = 82
	msgBTGATTWriteResponse           = 83
	msgBTGATTNotifyResponse          = 84
	msgBTDevicePairingResponse       = 85
	msgBTDeviceUnpairingResponse     = 86
	msgUnsubscribeBTAdvertisements   = 87
	msgBTDeviceClearCacheResponse    = 88
	msgBTRawAdvertisementsResponse   = 93
	msgNoiseEncryptionSetKeyRequest  = 124
	msgNoiseEncryptionSetKeyResponse = 125
	msgBTScannerStateResponse        = 126
	msgBTScannerSetModeRequest       = 127
	msgDeviceCapabilitiesRequest     = 149
	msgDeviceCapabilitiesResponse    = 150
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

// Scanner states and modes.
const (
	scannerRunning = 2
	scannerActive  = 1
)

// gattError is reported for failed GATT operations and connections (ESP_GATT_ERROR, 0x85).
const gattError = 0x85

type deviceInfo struct {
	Name, FriendlyName, MAC, BTMAC string
	Version, Model, Manufacturer   string
	ProjectName, ProjectVersion    string
	SuggestedArea                  string
	FeatureFlags                   uint32
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

func encRawAdvertisements(ads []ble.Advert) []byte {
	var e enc
	for _, a := range ads {
		var s enc
		s.uint(1, a.Addr)
		s.sint(2, int64(a.RSSI))
		s.uint(3, uint64(a.AddrType))
		s.bytes(4, a.Data)
		e.msg(1, s.b)
	}
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

// encUUID writes the 128-bit form (both halves, even zero ones: their position matters) and, for UUIDs on the
// Bluetooth base, the short form too.
func encUUID(e *enc, u ble.UUID, shortField protowire.Number) {
	for _, v := range []uint64{u.Hi, u.Lo} {
		e.b = protowire.AppendTag(e.b, 1, protowire.VarintType)
		e.b = protowire.AppendVarint(e.b, v)
	}
	if s, ok := u.Short(); ok {
		e.uint(shortField, uint64(s))
	}
}

// encServices encodes one GetServicesResponse holding the given services.
func encServices(addr uint64, services []ble.Service) []byte {
	var e enc
	e.uint(1, addr)
	for _, s := range services {
		var se enc
		encUUID(&se, s.UUID, 4)
		se.uint(2, uint64(s.Handle))
		for _, c := range s.Characteristics {
			var ce enc
			encUUID(&ce, c.UUID, 5)
			ce.uint(2, uint64(c.Handle))
			ce.uint(3, uint64(c.Properties))
			for _, d := range c.Descriptors {
				var de enc
				encUUID(&de, d.UUID, 3)
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

func encScannerState(state, mode, configured uint32) []byte {
	var e enc
	e.uint(1, uint64(state))
	e.uint(2, uint64(mode))
	e.uint(3, uint64(configured))
	return e.b
}

// request holds the fields shared by the Bluetooth requests: address, handle, request type, flags and data.
type request struct {
	addr     uint64
	handle   uint16
	kind     uint32 // request_type for device requests, flags for subscriptions
	addrType uint32
	flag     bool // response for writes, enable for notify
	data     []byte
}

// decRequest decodes a Bluetooth request; field numbers depend on the message type.
func decRequest(typ uint32, b []byte) (request, error) {
	fs, err := fields(b)
	if err != nil {
		return request{}, err
	}
	var r request
	for _, f := range fs {
		switch typ {
		case msgBTDeviceRequest:
			switch f.num {
			case 1:
				r.addr = f.v
			case 2:
				r.kind = uint32(f.v)
			case 4:
				r.addrType = uint32(f.v)
			}
		case msgSubscribeBTAdvertisements:
			if f.num == 1 {
				r.kind = uint32(f.v)
			}
		case msgBTGATTWriteRequest:
			switch f.num {
			case 1:
				r.addr = f.v
			case 2:
				r.handle = uint16(f.v)
			case 3:
				r.flag = f.v != 0
			case 4:
				r.data = f.data
			}
		default: // get services, read, read/write descriptor, notify
			switch f.num {
			case 1:
				r.addr = f.v
			case 2:
				r.handle = uint16(f.v)
			case 3:
				if typ == msgBTGATTNotifyRequest {
					r.flag = f.v != 0
				} else {
					r.data = f.data
				}
			}
		}
	}
	return r, nil
}
