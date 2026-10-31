# btproxy

An ESPHome-compatible Bluetooth proxy for Linux, written in Go. Home Assistant adds it through the ESPHome
integration (port 6053) exactly like an ESP32 proxy, then scans and connects to BLE devices through the host's
Bluetooth adapter. It talks to BlueZ over D-Bus, so the adapter stays usable by other programs at the same time.

Status: works end to end against `aioesphomeapi` (Home Assistant's client library) with the fake backend; the
BlueZ backend compiles and its unit tests pass, but it has not run against a real adapter yet.

## What it supports

- Raw advertisements, batched like ESPHome (rebuilt from BlueZ's parsed properties, see below).
- Active connections: connect, service discovery, read and write of characteristics and descriptors,
  notifications, disconnect, connection slot reporting, cache clearing. Up to `-connections` (default 3).
- Not supported: pairing, passive scanning (BlueZ needs its experimental advertisement monitor API for that,
  so the scanner always runs active and says so), connection parameter changes, API encryption (Noise), mDNS.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

The binaries are static, about 4 MB (4.4 MB for mipsle).

## Run

Needs `bluetoothd` and the system D-Bus, and permission to use the adapter (root, or a D-Bus policy for the
user). For devices BlueZ has not seen yet, connecting uses `ConnectDevice`, which needs `bluetoothd
--experimental`; devices in range are normally already known from scanning.

```sh
btproxy -name btproxy-bedroom -area Bedroom
```

Flags: `-name`, `-friendly-name`, `-area`, `-listen` (`:6053`), `-adapter` (`hci0`), `-connections` (3),
`-mac` (unique id reported to HA; default: the first network interface's MAC), `-backend` (`bluez` or `fake`),
`-debug`.

In Home Assistant: Settings, Devices & services, Add integration, ESPHome, host and port 6053. There is no mDNS
yet, so add it by IP.

## Notes

- BlueZ exposes advertisements only in parsed form (name, manufacturer data, service data, UUIDs, flags, TX
  power), so the raw AD structures are rebuilt from those and capped at 62 bytes. Field order differs from the
  air; Home Assistant parses the fields, so that does not matter. `UUIDs` can include services BlueZ learnt
  by connecting.
- Attribute handles come from BlueZ's object paths (`service000a/char000b/desc000d`).
- BlueZ owns the CCCD descriptors, so a write to one turns notifications on or off with
  `StartNotify`/`StopNotify` instead.
- Discovery pauses while a connection is being made, as ESPHome does.

## Test

```sh
go test ./...                       # the bluez tests need GOOS=linux (or a Linux container)
go run ./cmd/btproxy -backend fake -listen 127.0.0.1:16053 &
python -m venv .venv && .venv/bin/pip install aioesphomeapi
.venv/bin/python test/e2e.py 127.0.0.1 16053
```
