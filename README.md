# btproxy

An ESPHome-compatible Bluetooth proxy for Linux, written in Go. Home Assistant adds it through the ESPHome
integration (port 6053) exactly like an ESP32 proxy, then scans and connects to BLE devices through the host's
Bluetooth adapter. It talks to BlueZ over D-Bus, so the adapter stays usable by other programs at the same time.

Status: in daily use since 2026-10-31 on an OpenWrt 23.05 router (MT7620A, mipsle, 64 MB RAM) with a Realtek
RTL8761B USB stick, serving Home Assistant 2026.10: advertisements for device tracking plus an active, bonded
connection to a Tion breezer. The fake backend passes an end-to-end test with `aioesphomeapi` (Home Assistant's
client library).

## What it supports

- Raw advertisements, batched like ESPHome (rebuilt from BlueZ's parsed properties, see below).
- Active connections: connect, service discovery, read and write of characteristics and descriptors,
  notifications, disconnect, connection slot reporting, cache clearing. Up to `-connections` (default 3).
- Not supported: pairing, passive scanning (BlueZ needs its experimental advertisement monitor API for that,
  so the scanner always runs active and says so), connection parameter changes, API encryption (Noise), IPv6 mDNS.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

The binaries are static, about 4 MB (4.5 MB for mipsle).

## Run

Needs `bluetoothd` and the system D-Bus, and permission to use the adapter (root, or a D-Bus policy for the
user). For devices BlueZ has not seen yet, connecting uses `ConnectDevice`, which needs `bluetoothd
--experimental`; devices in range are normally already known from scanning.

```sh
btproxy -name btproxy-bedroom -area Bedroom
```

Flags: `-name`, `-friendly-name`, `-area`, `-listen` (`:6053`), `-adapter` (`hci0`), `-connections` (3),
`-mac` (unique id reported to HA; default: the first network interface's MAC), `-mdns` (`true`), `-mdns-iface`
(comma-separated interfaces to advertise on; default: all up interfaces with IPv4), `-backend` (`bluez` or
`fake`), `-debug`.

Home Assistant discovers the proxy over mDNS and offers it under Settings, Devices & services. Without mDNS
(`-mdns=false`, or a network that does not pass multicast), add it by hand: Add integration, ESPHome, host and
port 6053.

## mDNS

btproxy advertises itself like an ESPHome device: `_esphomelib._tcp` service `<name>` on host `<name>.local`
(`-name` reduced to a DNS label), with the TXT keys ESPHome's mdns component sends and Home Assistant reads:
`friendly_name`, `version` (the ESPHome version btproxy reports), `mac` (the `-mac` value, lower case without
colons), `platform=linux`, `board` (the Go architecture), `network=ethernet`, `project_name` and
`project_version`. There is no `api_encryption`, since the API is plaintext.

Home Assistant uses `mac` as the config entry's unique id, the same id DeviceInfo gives, so an entry added
earlier by IP is matched rather than duplicated. When the address changes, the next announcement updates the
entry's host, as long as Home Assistant has lost the connection by then (it keeps a working connection and
ignores the announcement otherwise).

The responder is built in (IPv4 only). It announces three times at start, again when an interface address
changes (checked every 30 s), and sends goodbye packets on SIGINT or SIGTERM. Each query is answered with the
addresses of the interface it came from; queries from other subnets are ignored. On a router with several VLANs,
restrict it with `-mdns-iface br-lan` so the proxy is not advertised on WAN or guest networks.

It shares port 5353 with other responders: the socket sets `SO_REUSEADDR` and `SO_REUSEPORT`, which Linux
accepts next to OpenWrt's `umdns` (`SO_REUSEADDR`) and `avahi-daemon`. Multicast reaches every socket; a
unicast packet to port 5353 reaches only one of them, which only matters for queriers that send unicast to
5353 instead of the group. Keep `-name` different from the host name, since `umdns` already answers for
`<hostname>.local` (the default, `btproxy-<hostname>`, is).

## Notes

- BlueZ exposes advertisements only in parsed form (name, manufacturer data, service data, UUIDs, flags, TX
  power), so the raw AD structures are rebuilt from those and capped at 62 bytes. Field order differs from the
  air; Home Assistant parses the fields, so that does not matter. `UUIDs` can include services BlueZ learnt
  by connecting.
- Attribute handles come from BlueZ's object paths (`service000a/char000b/desc000d`). BlueZ names a
  characteristic after its declaration handle; the proxy reports the value handle after it (`0x000c` here), as an
  ESP32 does, because Home Assistant shares one GATT cache per device between proxies.
- BlueZ owns the CCCD descriptors, so a write to one turns notifications on or off with
  `StartNotify`/`StopNotify` instead.
- Discovery keeps running while a connection is being made (ESPHome pauses it; BlueZ copes without).
- There is no idle timeout on API clients: Home Assistant does not ping while advertisements flow, so dead peers
  are found with TCP keepalive (30 s idle, then 3 probes 10 s apart).

## Deploy

OpenWrt (23.05): `opkg install kmod-bluetooth bluez-daemon` plus the firmware for the stick (for example
`rtl8761bu-firmware`), copy the build for the router's architecture (`dist/btproxy-mipsle` for MT7620/MT7621) to
`/usr/bin/btproxy`, and [contrib/openwrt/btproxy.init](contrib/openwrt/btproxy.init) to `/etc/init.d/btproxy`.
OpenWrt keeps BlueZ's state in `/var/lib/bluetooth` on tmpfs; link it to a directory on flash (for example
`/etc/bluetooth/keys`) so bonds survive a reboot, and list `/usr/bin/btproxy`, `/etc/init.d/btproxy` and that
directory in `/etc/sysupgrade.conf`.

systemd (Raspberry Pi OS, Debian): copy the binary to `/usr/local/bin/btproxy` and
[contrib/systemd/btproxy.service](contrib/systemd/btproxy.service) to `/etc/systemd/system/`, then
`systemctl enable --now btproxy`.

### Bonded devices

Devices that need an encrypted link (a Tion breezer, for example) must be bonded with the proxy's adapter.
Either pair once with `bluetoothctl`, or move an existing bond: stop `bluetoothd` on both machines, copy
`/var/lib/bluetooth/<adapter MAC>/<device MAC>/` (`info` and `attributes`) into the new adapter's directory,
then start `bluetoothd`. Copy only while `bluetoothd` is stopped, because it rewrites `info` from memory when it
exits.

## Test

```sh
go test ./...                       # the bluez and mDNS socket tests need GOOS=linux (or a Linux container)
go run ./cmd/btproxy -backend fake -listen 127.0.0.1:16053 &
python -m venv .venv && .venv/bin/pip install aioesphomeapi
.venv/bin/python test/e2e.py 127.0.0.1 16053
```
