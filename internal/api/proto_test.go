package api

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"

	"google.golang.org/protobuf/encoding/protowire"
)

// The protobuf code is hand-rolled, so nothing but these tests ties the message ids, field numbers and
// enum values in the Go code to proto/api.proto, ESPHome's schema. The schema is read with regular
// expressions: message names with their ids and fields, and enums with their values.

// protoSchema is what the tests need from api.proto.
type protoSchema struct {
	ids    map[string]int            // message name -> id
	fields map[string]map[string]int // message name -> field name -> number
	enums  map[string]map[string]int // enum name -> value name -> value
}

var (
	protoMessage = regexp.MustCompile(`(?ms)^message\s+(\w+)\s*\{(.*?)^\}`)
	protoEnum    = regexp.MustCompile(`(?ms)^enum\s+(\w+)\s*\{(.*?)^\}`)
	protoID      = regexp.MustCompile(`option\s*\(id\)\s*=\s*(\d+)`)
	protoField   = regexp.MustCompile(`(?m)^\s*(?:repeated\s+)?[\w.]+\s+(\w+)\s*=\s*(\d+)`)
	protoValue   = regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*(-?\d+)`)
)

func loadSchema(t *testing.T) *protoSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "proto", "api.proto"))
	if err != nil {
		t.Fatal(err)
	}
	sc := &protoSchema{ids: map[string]int{}, fields: map[string]map[string]int{}, enums: map[string]map[string]int{}}
	for _, m := range protoMessage.FindAllSubmatch(b, -1) {
		name, body := string(m[1]), m[2]
		if id := protoID.FindSubmatch(body); id != nil {
			sc.ids[name], _ = strconv.Atoi(string(id[1]))
		}
		fs := map[string]int{}
		for _, f := range protoField.FindAllSubmatch(body, -1) {
			if fname := string(f[1]); fname != "option" && fname != "reserved" {
				fs[fname], _ = strconv.Atoi(string(f[2]))
			}
		}
		sc.fields[name] = fs
	}
	for _, m := range protoEnum.FindAllSubmatch(b, -1) {
		vs := map[string]int{}
		for _, v := range protoValue.FindAllSubmatch(m[2], -1) {
			vs[string(v[1])], _ = strconv.Atoi(string(v[2]))
		}
		sc.enums[string(m[1])] = vs
	}
	if len(sc.ids) < 100 || len(sc.enums) < 10 {
		t.Fatalf("parsed %d message ids and %d enums from api.proto", len(sc.ids), len(sc.enums))
	}
	return sc
}

// num is the number of a field of a message, which must exist.
func (sc *protoSchema) num(t *testing.T, msg, field string) int {
	t.Helper()
	n, ok := sc.fields[msg][field]
	if !ok {
		t.Fatalf("api.proto has no field %s.%s", msg, field)
	}
	return n
}

// encode builds a message from field names: uint64 and bool as varints, string and []byte as bytes.
func (sc *protoSchema) encode(t *testing.T, msg string, vals map[string]any) []byte {
	t.Helper()
	var e enc
	for name, v := range vals {
		n := protowire.Number(sc.num(t, msg, name))
		switch v := v.(type) {
		case uint64:
			e.uint(n, v)
		case bool:
			e.bool(n, v)
		case string:
			e.str(n, v)
		case []byte:
			e.bytes(n, v)
		default:
			t.Fatalf("%s.%s: cannot encode %T", msg, name, v)
		}
	}
	return e.b
}

// protoMsg is what an encoded message must hold, by field name: uint64 for varint and fixed fields (an
// absent field reads as 0), string or []byte for length-delimited ones, protoMsg for a sub-message, and
// []uint64 or []protoMsg for a repeated field, in order.
type protoMsg struct {
	name string
	want map[string]any
}

// check decodes payload and compares it with m. A field the schema does not declare is an error too.
func (sc *protoSchema) check(t *testing.T, m protoMsg, payload []byte) {
	t.Helper()
	nums, ok := sc.fields[m.name]
	if !ok {
		t.Fatalf("api.proto has no message %s", m.name)
	}
	if _, ok := sc.ids[m.name]; !ok && len(nums) == 0 {
		t.Fatalf("api.proto message %s has no fields", m.name)
	}
	fs, err := fields(payload)
	if err != nil {
		t.Fatalf("%s: %v", m.name, err)
	}
	known := map[int]bool{}
	for _, n := range nums {
		known[n] = true
	}
	byNum := map[int][]field{}
	for _, f := range fs {
		if !known[int(f.num)] {
			t.Errorf("%s: encoded field %d is not in api.proto", m.name, f.num)
		}
		byNum[int(f.num)] = append(byNum[int(f.num)], f)
	}
	for name, want := range m.want {
		got := byNum[sc.num(t, m.name, name)]
		var first field
		if len(got) > 0 {
			first = got[0]
		}
		switch want := want.(type) {
		case uint64:
			if first.v != want {
				t.Errorf("%s.%s = %d, want %d", m.name, name, first.v, want)
			}
		case string:
			if string(first.data) != want {
				t.Errorf("%s.%s = %q, want %q", m.name, name, first.data, want)
			}
		case []byte:
			if !bytes.Equal(first.data, want) {
				t.Errorf("%s.%s = %x, want %x", m.name, name, first.data, want)
			}
		case protoMsg:
			if len(got) != 1 {
				t.Errorf("%s.%s: %d values, want one message", m.name, name, len(got))
				continue
			}
			sc.check(t, want, first.data)
		case []uint64:
			if len(got) != len(want) {
				t.Errorf("%s.%s: %d values, want %d", m.name, name, len(got), len(want))
				continue
			}
			for i, f := range got {
				if f.v != want[i] {
					t.Errorf("%s.%s[%d] = %d, want %d", m.name, name, i, f.v, want[i])
				}
			}
		case []protoMsg:
			if len(got) != len(want) {
				t.Errorf("%s.%s: %d messages, want %d", m.name, name, len(got), len(want))
				continue
			}
			for i, f := range got {
				sc.check(t, want[i], f.data)
			}
		default:
			t.Fatalf("%s.%s: cannot check %T", m.name, name, want)
		}
	}
}

// TestProtoMessageIDs checks the message type constants.
func TestProtoMessageIDs(t *testing.T) {
	sc := loadSchema(t)
	for _, c := range []struct {
		name string
		id   int
	}{
		{"HelloRequest", msgHelloRequest},
		{"HelloResponse", msgHelloResponse},
		{"DisconnectRequest", msgDisconnectRequest},
		{"DisconnectResponse", msgDisconnectResponse},
		{"PingRequest", msgPingRequest},
		{"PingResponse", msgPingResponse},
		{"DeviceInfoRequest", msgDeviceInfoRequest},
		{"DeviceInfoResponse", msgDeviceInfoResponse},
		{"ListEntitiesRequest", msgListEntitiesRequest},
		{"ListEntitiesBinarySensorResponse", msgListEntitiesBinarySensorResponse},
		{"ListEntitiesSensorResponse", msgListEntitiesSensorResponse},
		{"ListEntitiesSwitchResponse", msgListEntitiesSwitchResponse},
		{"ListEntitiesTextSensorResponse", msgListEntitiesTextSensorResponse},
		{"ListEntitiesDoneResponse", msgListEntitiesDoneResponse},
		{"SubscribeStatesRequest", msgSubscribeStatesRequest},
		{"BinarySensorStateResponse", msgBinarySensorStateResponse},
		{"SensorStateResponse", msgSensorStateResponse},
		{"SwitchStateResponse", msgSwitchStateResponse},
		{"TextSensorStateResponse", msgTextSensorStateResponse},
		{"SubscribeLogsRequest", msgSubscribeLogsRequest},
		{"SubscribeLogsResponse", msgSubscribeLogsResponse},
		{"SwitchCommandRequest", msgSwitchCommandRequest},
		{"SubscribeHomeassistantServicesRequest", msgSubscribeHAServicesRequest},
		{"GetTimeResponse", msgGetTimeResponse},
		{"SubscribeHomeAssistantStatesRequest", msgSubscribeHAStatesRequest},
		{"ListEntitiesButtonResponse", msgListEntitiesButtonResponse},
		{"ButtonCommandRequest", msgButtonCommandRequest},
		{"SubscribeBluetoothLEAdvertisementsRequest", msgSubscribeBTAdvertisements},
		{"BluetoothDeviceRequest", msgBTDeviceRequest},
		{"BluetoothDeviceConnectionResponse", msgBTDeviceConnectionResponse},
		{"BluetoothGATTGetServicesRequest", msgBTGATTGetServicesRequest},
		{"BluetoothGATTGetServicesResponse", msgBTGATTGetServicesResponse},
		{"BluetoothGATTGetServicesDoneResponse", msgBTGATTGetServicesDoneResponse},
		{"BluetoothGATTReadRequest", msgBTGATTReadRequest},
		{"BluetoothGATTReadResponse", msgBTGATTReadResponse},
		{"BluetoothGATTWriteRequest", msgBTGATTWriteRequest},
		{"BluetoothGATTReadDescriptorRequest", msgBTGATTReadDescriptorRequest},
		{"BluetoothGATTWriteDescriptorRequest", msgBTGATTWriteDescriptorRequest},
		{"BluetoothGATTNotifyRequest", msgBTGATTNotifyRequest},
		{"BluetoothGATTNotifyDataResponse", msgBTGATTNotifyDataResponse},
		{"SubscribeBluetoothConnectionsFreeRequest", msgSubscribeBTConnectionsFree},
		{"BluetoothConnectionsFreeResponse", msgBTConnectionsFreeResponse},
		{"BluetoothGATTErrorResponse", msgBTGATTErrorResponse},
		{"BluetoothGATTWriteResponse", msgBTGATTWriteResponse},
		{"BluetoothGATTNotifyResponse", msgBTGATTNotifyResponse},
		{"BluetoothDevicePairingResponse", msgBTDevicePairingResponse},
		{"BluetoothDeviceUnpairingResponse", msgBTDeviceUnpairingResponse},
		{"UnsubscribeBluetoothLEAdvertisementsRequest", msgUnsubscribeBTAdvertisements},
		{"BluetoothDeviceClearCacheResponse", msgBTDeviceClearCacheResponse},
		{"BluetoothLERawAdvertisementsResponse", msgBTRawAdvertisementsResponse},
		{"NoiseEncryptionSetKeyRequest", msgNoiseEncryptionSetKeyRequest},
		{"NoiseEncryptionSetKeyResponse", msgNoiseEncryptionSetKeyResponse},
		{"BluetoothScannerStateResponse", msgBTScannerStateResponse},
		{"BluetoothScannerSetModeRequest", msgBTScannerSetModeRequest},
		{"BluetoothSetConnectionParamsRequest", msgBTSetConnParamsRequest},
		{"BluetoothSetConnectionParamsResponse", msgBTSetConnParamsResponse},
		{"DeviceCapabilitiesRequest", msgDeviceCapabilitiesRequest},
		{"DeviceCapabilitiesResponse", msgDeviceCapabilitiesResponse},
	} {
		if id, ok := sc.ids[c.name]; !ok {
			t.Errorf("api.proto has no message %s", c.name)
		} else if id != c.id {
			t.Errorf("%s has id %d in api.proto, %d in the code", c.name, id, c.id)
		}
	}
	// AuthenticationRequest, which older clients send, is gone from the schema: 3 is ignored by number.
	if _, ok := handlers[3]; !ok {
		t.Error("message 3 is not ignored")
	}
}

// TestProtoEnums checks the constants taken from the schema's enums.
func TestProtoEnums(t *testing.T) {
	sc := loadSchema(t)
	for _, c := range []struct {
		enum, name string
		value      int
	}{
		{"LogLevel", "LOG_LEVEL_NONE", logNone},
		{"LogLevel", "LOG_LEVEL_ERROR", logError},
		{"LogLevel", "LOG_LEVEL_WARN", logWarn},
		{"LogLevel", "LOG_LEVEL_INFO", logInfo},
		{"LogLevel", "LOG_LEVEL_CONFIG", logConfig},
		{"LogLevel", "LOG_LEVEL_DEBUG", logDebug},
		{"LogLevel", "LOG_LEVEL_VERBOSE", logVerbose},
		{"LogLevel", "LOG_LEVEL_VERY_VERBOSE", logVeryVerbose},
		{"EntityCategory", "ENTITY_CATEGORY_CONFIG", entityConfig},
		{"EntityCategory", "ENTITY_CATEGORY_DIAGNOSTIC", entityDiagnostic},
		{"SensorStateClass", "STATE_CLASS_MEASUREMENT", stateMeasurement},
		{"SensorStateClass", "STATE_CLASS_TOTAL_INCREASING", stateTotalIncreasing},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_DISCONNECT", reqDisconnect},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_PAIR", reqPair},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_UNPAIR", reqUnpair},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT_V3_WITH_CACHE", reqConnectV3WithCache},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT_V3_WITHOUT_CACHE", reqConnectV3WithoutCache},
		{"BluetoothDeviceRequestType", "BLUETOOTH_DEVICE_REQUEST_TYPE_CLEAR_CACHE", reqClearCache},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_IDLE", int(ble.ScanIdle)},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_STARTING", int(ble.ScanStarting)},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_RUNNING", int(ble.ScanRunning)},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_FAILED", int(ble.ScanFailed)},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_STOPPING", int(ble.ScanStopping)},
		{"BluetoothScannerState", "BLUETOOTH_SCANNER_STATE_STOPPED", int(ble.ScanStopped)},
		{"BluetoothScannerMode", "BLUETOOTH_SCANNER_MODE_PASSIVE", int(ble.ScanPassive)},
		{"BluetoothScannerMode", "BLUETOOTH_SCANNER_MODE_ACTIVE", int(ble.ScanActive)},
	} {
		if v, ok := sc.enums[c.enum][c.name]; !ok {
			t.Errorf("api.proto has no %s.%s", c.enum, c.name)
		} else if v != c.value {
			t.Errorf("%s.%s is %d in api.proto, %d in the code", c.enum, c.name, v, c.value)
		}
	}
}

// TestProtoEncoders encodes a sample of every message the proxy sends and checks each value sits in the
// field api.proto gives that name.
func TestProtoEncoders(t *testing.T) {
	sc := loadSchema(t)
	base := ble.UUID{Hi: 0x2a19<<32 | 0x1000, Lo: 0x800000805f9b34fb} // on the Bluetooth base: short form
	custom := ble.UUID{Hi: 0x6e400001b5a34f8d, Lo: 0x0003c8dd2a4d3e49}
	services := []ble.Service{{UUID: custom, Handle: 1, Characteristics: []ble.Characteristic{
		{UUID: base, Handle: 2, Properties: 0x12, Descriptors: []ble.Descriptor{{UUID: custom, Handle: 3}}},
	}}}
	sensor := &sensor{entityHeader: entityHeader{"id", "Name", "mdi:x"}, unit: "s", class: "duration",
		accuracy: 1, stateClass: stateMeasurement}
	binary := &binarySensor{entityHeader: entityHeader{"id", "Name", "mdi:x"}, class: "running",
		value: func() bool { return true }}
	text := &textSensor{entityHeader: entityHeader{"id", "Name", "mdi:x"}, value: func() string { return "t" }}
	sw := &scanSwitch{entityHeader{"id", "Name", "mdi:x"}, NewServer(Config{Name: "t"}, testAdapter{}, logx.Discard())}
	bt := &button{entityHeader{"id", "Name", "mdi:x"}, nil}
	key := uint64(entityKey("id"))
	pick := func(typ uint32, payload []byte) []byte { return payload }
	pick3 := func(typ uint32, payload []byte, _ bool) []byte { return payload }
	for _, c := range []struct {
		msg     protoMsg
		payload []byte
	}{
		{protoMsg{"HelloResponse", map[string]any{"api_version_major": uint64(apiMajor),
			"api_version_minor": uint64(apiMinor), "server_info": "info", "name": "node"}},
			encHelloResponse("info", "node")},
		{protoMsg{"DeviceInfoResponse", map[string]any{"name": "n", "mac_address": "m", "esphome_version": "v",
			"model": "mo", "project_name": "pn", "project_version": "pv", "manufacturer": "ma",
			"friendly_name": "f", "bluetooth_proxy_feature_flags": uint64(0x55), "suggested_area": "a",
			"bluetooth_mac_address": "b", "api_encryption_supported": uint64(1)}},
			encDeviceInfo(deviceInfo{Name: "n", FriendlyName: "f", MAC: "m", BTMAC: "b", Version: "v", Model: "mo",
				Manufacturer: "ma", ProjectName: "pn", ProjectVersion: "pv", SuggestedArea: "a", FeatureFlags: 0x55,
				EncryptionSupported: true})},
		{protoMsg{"DeviceCapabilitiesResponse", map[string]any{"bluetooth_proxy": protoMsg{"BluetoothProxyCapabilities",
			map[string]any{"feature_flags": uint64(0x55), "mac_address": "b"}}}}, encCapabilities(0x55, "b")},
		{protoMsg{"BluetoothLERawAdvertisementsResponse", map[string]any{"advertisements": []protoMsg{
			{"BluetoothLERawAdvertisement", map[string]any{"address": uint64(1), "rssi": protowire.EncodeZigZag(-50),
				"address_type": uint64(ble.AddrRandom), "data": []byte{2, 1, 6}}}}}},
			appendRawAdvert(nil, ble.Advert{Addr: 1, RSSI: -50, AddrType: ble.AddrRandom, Data: []byte{2, 1, 6}})},
		{protoMsg{"BluetoothDeviceConnectionResponse", map[string]any{"address": uint64(1), "connected": uint64(1),
			"mtu": uint64(23), "error": uint64(gattError)}}, encConnection(1, true, 23, gattError)},
		{protoMsg{"BluetoothConnectionsFreeResponse", map[string]any{"free": uint64(1), "limit": uint64(3),
			"allocated": []uint64{7, 8}}}, encConnectionsFree(1, 3, []uint64{7, 8})},
		{protoMsg{"BluetoothGATTGetServicesResponse", map[string]any{"address": uint64(1), "services": []protoMsg{
			{"BluetoothGATTService", map[string]any{"uuid": []uint64{custom.Hi, custom.Lo}, "handle": uint64(1),
				"characteristics": []protoMsg{{"BluetoothGATTCharacteristic", map[string]any{
					"short_uuid": uint64(0x2a19), "handle": uint64(2), "properties": uint64(0x12),
					"descriptors": []protoMsg{{"BluetoothGATTDescriptor", map[string]any{
						"uuid": []uint64{custom.Hi, custom.Lo}, "handle": uint64(3)}}}}}}}}}}},
			encServices(1, services, true)},
		{protoMsg{"BluetoothGATTGetServicesDoneResponse", map[string]any{"address": uint64(1)}}, encAddrHandle(1, 0)},
		{protoMsg{"BluetoothGATTWriteResponse", map[string]any{"address": uint64(1), "handle": uint64(2)}},
			encAddrHandle(1, 2)},
		{protoMsg{"BluetoothGATTNotifyResponse", map[string]any{"address": uint64(1), "handle": uint64(2)}},
			encAddrHandle(1, 2)},
		{protoMsg{"BluetoothGATTReadResponse", map[string]any{"address": uint64(1), "handle": uint64(2),
			"data": []byte{3}}}, encAddrHandleData(1, 2, []byte{3})},
		{protoMsg{"BluetoothGATTNotifyDataResponse", map[string]any{"address": uint64(1), "handle": uint64(2),
			"data": []byte{3}}}, encAddrHandleData(1, 2, []byte{3})},
		{protoMsg{"BluetoothGATTErrorResponse", map[string]any{"address": uint64(1), "handle": uint64(2),
			"error": uint64(gattError)}}, encGATTError(1, 2, gattError)},
		{protoMsg{"BluetoothDevicePairingResponse", map[string]any{"address": uint64(1), "paired": uint64(0),
			"error": uint64(gattError)}}, encAddrResult(1, false, gattError)},
		{protoMsg{"BluetoothDeviceUnpairingResponse", map[string]any{"address": uint64(1), "success": uint64(1)}},
			encAddrResult(1, true, 0)},
		{protoMsg{"BluetoothDeviceClearCacheResponse", map[string]any{"address": uint64(1), "success": uint64(1)}},
			encAddrResult(1, true, 0)},
		{protoMsg{"BluetoothSetConnectionParamsResponse", map[string]any{"address": uint64(1),
			"error": uint64(gattError)}}, encConnParamsResult(1, gattError)},
		{protoMsg{"BluetoothScannerStateResponse", map[string]any{"state": uint64(ble.ScanRunning),
			"mode": uint64(ble.ScanActive), "configured_mode": uint64(ble.ScanPassive)}},
			encScannerState(ble.ScanRunning, ble.ScanActive, ble.ScanPassive)},
		{protoMsg{"SubscribeLogsResponse", map[string]any{"level": uint64(logWarn), "message": []byte("x")}},
			encLog(logWarn, []byte("x"))},
		{protoMsg{"ListEntitiesSensorResponse", map[string]any{"object_id": "id", "key": key, "name": "Name",
			"icon": "mdi:x", "unit_of_measurement": "s", "accuracy_decimals": uint64(1), "device_class": "duration",
			"state_class": uint64(stateMeasurement), "entity_category": uint64(entityDiagnostic)}},
			pick(sensor.info())},
		{protoMsg{"SensorStateResponse", map[string]any{"key": uint64(5), "state": uint64(math.Float32bits(1.5)),
			"missing_state": uint64(0)}}, encSensorState(5, 1.5, true)},
		{protoMsg{"SensorStateResponse", map[string]any{"key": uint64(5), "missing_state": uint64(1)}},
			encSensorState(5, 0, false)},
		{protoMsg{"ListEntitiesBinarySensorResponse", map[string]any{"object_id": "id", "key": key, "name": "Name",
			"icon": "mdi:x", "device_class": "running", "entity_category": uint64(entityDiagnostic)}},
			pick(binary.info())},
		{protoMsg{"BinarySensorStateResponse", map[string]any{"key": key, "state": uint64(1)}}, pick3(binary.state())},
		{protoMsg{"ListEntitiesTextSensorResponse", map[string]any{"object_id": "id", "key": key, "name": "Name",
			"icon": "mdi:x", "entity_category": uint64(entityDiagnostic)}}, pick(text.info())},
		{protoMsg{"TextSensorStateResponse", map[string]any{"key": key, "state": "t"}}, pick3(text.state())},
		{protoMsg{"ListEntitiesSwitchResponse", map[string]any{"object_id": "id", "key": key, "name": "Name",
			"icon": "mdi:x", "entity_category": uint64(entityConfig)}}, pick(sw.info())},
		{protoMsg{"SwitchStateResponse", map[string]any{"key": key, "state": uint64(1)}}, pick3(sw.state())},
		{protoMsg{"ListEntitiesButtonResponse", map[string]any{"object_id": "id", "key": key, "name": "Name",
			"icon": "mdi:x", "disabled_by_default": uint64(1), "entity_category": uint64(entityConfig),
			"device_class": "restart"}}, pick(bt.info())},
	} {
		sc.check(t, c.msg, c.payload)
	}
}

// TestProtoDecoders builds the messages the proxy reads from the field names in api.proto and checks the
// decoders read them.
func TestProtoDecoders(t *testing.T) {
	sc := loadSchema(t)
	if major, minor, err := decHello(sc.encode(t, "HelloRequest",
		map[string]any{"api_version_major": uint64(1), "api_version_minor": uint64(12)})); err != nil || major != 1 || minor != 12 {
		t.Errorf("decHello: %d.%d, %v", major, minor, err)
	}
	if level, dump, err := decSubscribeLogs(sc.encode(t, "SubscribeLogsRequest",
		map[string]any{"level": uint64(logDebug), "dump_config": true})); err != nil || level != logDebug || !dump {
		t.Errorf("decSubscribeLogs: %d %v, %v", level, dump, err)
	}
	addr, p, err := decConnParams(sc.encode(t, "BluetoothSetConnectionParamsRequest", map[string]any{
		"address": uint64(1), "min_interval": uint64(2), "max_interval": uint64(3), "latency": uint64(4), "timeout": uint64(5)}))
	if err != nil || addr != 1 || p != (ble.ConnParams{MinInterval: 2, MaxInterval: 3, Latency: 4, Timeout: 5}) {
		t.Errorf("decConnParams: %d %+v, %v", addr, p, err)
	}

	// The requests decRequest reads through reqLayouts: every field in a layout must be the one the schema
	// gives that number, and nothing else.
	for _, c := range []struct {
		typ    uint32
		name   string
		fields map[string]reqField
	}{
		{msgBTDeviceRequest, "BluetoothDeviceRequest",
			map[string]reqField{"address": fieldAddr, "request_type": fieldKind, "address_type": fieldAddrType}},
		{msgSubscribeBTAdvertisements, "SubscribeBluetoothLEAdvertisementsRequest", map[string]reqField{"flags": fieldKind}},
		{msgBTScannerSetModeRequest, "BluetoothScannerSetModeRequest", map[string]reqField{"mode": fieldKind}},
		{msgBTGATTGetServicesRequest, "BluetoothGATTGetServicesRequest", map[string]reqField{"address": fieldAddr}},
		{msgBTGATTReadRequest, "BluetoothGATTReadRequest", map[string]reqField{"address": fieldAddr, "handle": fieldHandle}},
		{msgBTGATTWriteRequest, "BluetoothGATTWriteRequest",
			map[string]reqField{"address": fieldAddr, "handle": fieldHandle, "response": fieldFlag, "data": fieldData}},
		{msgBTGATTReadDescriptorRequest, "BluetoothGATTReadDescriptorRequest",
			map[string]reqField{"address": fieldAddr, "handle": fieldHandle}},
		{msgBTGATTWriteDescriptorRequest, "BluetoothGATTWriteDescriptorRequest",
			map[string]reqField{"address": fieldAddr, "handle": fieldHandle, "data": fieldData}},
		{msgBTGATTNotifyRequest, "BluetoothGATTNotifyRequest",
			map[string]reqField{"address": fieldAddr, "handle": fieldHandle, "enable": fieldFlag}},
	} {
		layout := reqLayouts[c.typ]
		set := 0
		for _, f := range layout {
			if f != fieldNone {
				set++
			}
		}
		if set != len(c.fields) {
			t.Errorf("%s: layout sets %d fields, the test names %d", c.name, set, len(c.fields))
		}
		for name, want := range c.fields {
			n := sc.num(t, c.name, name)
			if n >= len(layout) || layout[n] != want {
				t.Errorf("%s.%s is field %d in api.proto, which the layout does not read as expected", c.name, name, n)
			}
		}
	}

	// The switch and button commands read their fields inline.
	restarted := make(chan struct{}, 1)
	s := NewServer(Config{Name: "t", Restart: func() { restarted <- struct{}{} }}, testAdapter{}, logx.Discard())
	c := &client{s: s, c: discardConn{}, log: s.log}
	c.out.init(c.c, c.log, false)
	c.switchCommand(Frame{Type: msgSwitchCommandRequest, Payload: sc.encode(t, "SwitchCommandRequest",
		map[string]any{"key": uint64(entityKey(scanSwitchID)), "state": false})})
	s.mu.Lock()
	off := s.scanOff
	s.mu.Unlock()
	if !off {
		t.Error("SwitchCommandRequest did not switch scanning off")
	}
	c.buttonCommand(Frame{Type: msgButtonCommandRequest, Payload: sc.encode(t, "ButtonCommandRequest",
		map[string]any{"key": uint64(entityKey("restart_proxy"))})})
	waitFor(t, "the restart button", func() bool { return len(restarted) == 1 })
}
