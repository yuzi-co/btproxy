# btproxy

An ESPHome-compatible Bluetooth proxy for Linux, written in Go. Home Assistant adds it through the ESPHome
integration (port 6053) exactly like an ESP32 proxy, then scans and connects to BLE devices through the host's
Bluetooth adapter. It talks to BlueZ over D-Bus, so the adapter stays usable by other programs at the same time.

Status: in daily use since 2026-10-31 on an OpenWrt 23.05 router (MT7620A, mipsle, 64 MB RAM) with a Realtek
RTL8761B USB stick, serving Home Assistant 2026.10: advertisements for device tracking plus an active, bonded
connection to a Tion breezer. The fake backend passes an end-to-end test with `aioesphomeapi` (Home Assistant's
client library).

## What it supports

- Raw advertisements, batched like ESPHome: read from the controller's HCI events when the proxy may open the
  kernel's HCI monitor, otherwise rebuilt from BlueZ's parsed properties (see
  [Advertisements](#advertisements)).
- Active connections: connect, service discovery, read and write of characteristics and descriptors,
  notifications, disconnect, connection slot reporting, cache clearing. Up to `-connections` (default 3).
- Pairing and unpairing on request from Home Assistant ("Just Works" only, see [Bonded devices](#bonded-devices)).
- Active and passive scanning, scanner state and mode reporting, and mode changes from Home Assistant (see
  [Scanning](#scanning)).
- Recovery without a restart when `bluetoothd` restarts, the adapter disappears and returns, or it is powered
  off: connections are reported dropped, then the adapter is powered on, scanning resumes and, after a
  `bluetoothd` restart, the pairing agent is registered again. A `bluetoothd` that hangs is detected too (see
  [Scanning](#scanning)) and, if `-bluetoothd-restart` is set, restarted.
- Not supported: connection parameter changes, API encryption (Noise), IPv6 mDNS.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

The binaries are static, about 4 MB (4.7 MB for mipsle).

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
`fake`), `-agent` (`true`: register a BlueZ pairing agent), `-scan-mode` (`active` or `passive`; default
`active`, like ESPHome), `-stall-timeout` (`3m`: restart active scanning after this long without
advertisements; `0` disables), `-bluetoothd-restart` (shell command that restarts `bluetoothd` when it hangs,
for example `"/etc/init.d/bluetoothd restart"` or `"systemctl restart bluetooth"`; default: none),
`-raw-adverts` (`auto`, `on` or `off`: where advertisements come from, see [Advertisements](#advertisements);
default `auto`), `-debug`.

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

## Advertisements

With `-raw-adverts auto` (the default) or `on`, the proxy opens the kernel's HCI monitor channel, the read-only
socket `btmon` uses, and takes advertisements from the controller's LE Advertising Report and LE Extended
Advertising Report events. Home Assistant then gets the bytes that were on the air: field order, address type,
AD types BlueZ does not parse, and every report the controller passes up, including repeats BlueZ would have
dropped as unchanged. `bluetoothd` keeps running the controller: it still starts and stops scanning and makes
the connections; the monitor only listens. Without the monitor (`-raw-adverts off`, or `auto` when it cannot be
opened), advertisements are rebuilt from BlueZ's parsed properties as before (see [Notes](#notes)).

The monitor needs `CAP_NET_RAW` (root has it) and a kernel with Bluetooth sockets, which any kernel that runs
BlueZ has. `auto` falls back to BlueZ's properties when the socket cannot be opened, when reading it fails, and
when it delivers nothing while BlueZ reports 30 advertisements; `on` refuses to start without it but falls
back on later failures all the same. The log says which source is in use. The proxy reads only the
`-adapter` controller (`hciN` is controller index N), and a socket filter in the kernel passes it only
advertising reports, so connection traffic does not wake it up.

Reports are passed on the way an ESP32 proxy passes them. During active scanning, an ESP32 holds the
advertisement of a scannable device until the scan response arrives and sends both as one entry, data
concatenated, with the response's RSSI; the proxy does the same, but sends a held advertisement alone if no
response follows within 200 ms or the device advertises again first. Other advertisements go out at once. An
entry carries at most 62 bytes; longer extended advertisements keep the AD structures that fit, and only their
first fragment is read. Anonymous advertisements and reports without RSSI are skipped. Address types are
reported as public or random; for a controller-resolved private address, that of the identity address.

How many advertisements reach the monitor is up to the controller. For active discovery the kernel turns the
controller's duplicate filter on, so it may report a device about once per scan (controllers differ in what
counts as a duplicate), except while an advertisement monitor is registered. With the HCI monitor in use, the
proxy therefore also registers its advertisement monitors during active scanning, when `bluetoothd` has the
monitor API (`--experimental`, see [Scanning](#scanning)), and asks BlueZ for discovery without
`DuplicateData`, which it no longer needs. Passive scanning has no scan responses either way. Unlike
`bluetoothd`, the monitor does not resolve private addresses: a bonded device that uses resolvable private
addresses shows up under its current random address, not its identity address, unless the controller resolves
it.

## Scanning

`-scan-mode` is the configured mode Home Assistant reads back. Home Assistant can switch the mode at run time
(its auto mode keeps a proxy passive and opens short active windows); the state reports carry the mode really
in use, which is active whenever passive scanning is unavailable. States: running, idle (no subscriber), failed
(`bluetoothd` or the adapter is gone, scanning could not start, `bluetoothd` did not answer, or scanning
stalled; retried every 10 s), stopped (the adapter is powered off and could not be powered on).

`bluetoothd` can hang while keeping its D-Bus name (seen with BlueZ 5.66 and an RTL8761B: `StartDiscovery`
never replied). Every call the proxy makes to it that should be answered at once gives up after 10 s, which
counts as a failure; GATT operations, connecting and pairing keep their own 30 s limits. In active mode a
watchdog also stops and restarts discovery when no advertisement at all has arrived for `-stall-timeout`
(3 minutes), reporting failed in between. It does not run in passive mode, where BlueZ reports a device only
when its advertisement changes and long silences are normal. After two stalls or unanswered calls in a row,
with no advertisement in between, the proxy runs the `-bluetoothd-restart` command (through `/bin/sh -c`, at
most once every 10 minutes, killed after 30 s); the recovery from the restart then brings scanning back. The
init script and the systemd unit in [contrib](contrib) set it. Without the command, the proxy keeps retrying
and reporting failed.

Active scanning is BlueZ discovery (`StartDiscovery`, LE only, duplicate data on). Passive scanning registers
advertisement monitors (`AdvertisementMonitorManager1`), with which the kernel scans passively and BlueZ
updates the devices that match. This API needs BlueZ 5.56 or later, a kernel with advertisement monitor
support (5.10 or later), and `bluetoothd --experimental` (`-E`): even current BlueZ registers the monitor
manager only in experimental mode. OpenWrt's `bluez-daemon` init script starts `bluetoothd -n` without it, so
there the proxy falls back to active scanning unless `/etc/init.d/bluetoothd` is changed to pass `-E` (which
also enables `ConnectDevice`): `procd_set_param command "$PROG" -n -E`, and list the file in
`/etc/sysupgrade.conf`. After `bluetoothd` restarts, passive scanning is tried again.

Passive scanning has limits that active scanning does not. The first two apply to advertisements rebuilt from
BlueZ's properties; the HCI monitor sees what the controller reports, which, unless the controller filters
monitor patterns itself (Microsoft's HCI extension, which some Intel and Realtek controllers have), is every
advertisement in range; how often depends on the kernel's duplicate filter setting for passive scans, which
5.15 turns off while monitors are registered and 6.x leaves on.

- BlueZ has no match-all monitor, and 5.66 overflows a buffer for monitors with more than 7 patterns, so the
  proxy registers three: common Flags values, manufacturer data of common company ids (Apple, Microsoft,
  Samsung, Google, Ruuvi, Govee, SwitchBot) and common 16-bit service data (Xiaomi, BTHome, SwitchBot,
  environmental sensing, Google, Tile). Advertisements without Flags that match none of these are not seen.
- Without discovery, BlueZ reports a device only when its advertisement content changes or its RSSI moves by
  8 dB or more, so unchanged beacons arrive far less often and RSSI is coarse. Prefer active scanning for
  presence detection.
- No scan responses, so names and other scan-response data of active-only devices are missing.

## Notes

- Without the HCI monitor (see [Advertisements](#advertisements)): BlueZ exposes advertisements only in parsed
  form (name, manufacturer data, service data, UUIDs, flags, TX power), so the raw AD structures are rebuilt
  from those and capped at 62 bytes. Field order differs from the
  air; Home Assistant parses the fields, so that does not matter. `UUIDs` can include services BlueZ learnt
  by connecting.
- Attribute handles come from BlueZ's object paths (`service000a/char000b/desc000d`). BlueZ names a
  characteristic after its declaration handle; the proxy reports the value handle after it (`0x000c` here), as an
  ESP32 does, because Home Assistant shares one GATT cache per device between proxies.
- BlueZ owns the CCCD descriptors, so a write to one turns notifications on or off with
  `StartNotify`/`StopNotify` instead.
- Discovery keeps running while a connection is being made (ESPHome pauses it; BlueZ copes without).
  Connecting does not depend on discovery: for `Device1.Connect` and `ConnectDevice` the kernel scans for the
  device itself, so connections work in passive mode too.
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
`systemctl enable --now btproxy`. The unit wants `bluetooth.service` rather than requiring it, so a
`bluetoothd` restart leaves the proxy running and it recovers by itself.

### Bonded devices

Devices that need an encrypted link (a Tion breezer, for example) must be bonded with the proxy's adapter.
Home Assistant can pair a connected device through the proxy (an integration asks for it, or the ESPHome
`bluetooth_device_pair` call): the proxy calls BlueZ's `Device1.Pair`, and a device that is already paired is
reported as paired without pairing again. The proxy never pairs on its own, so existing bonds are left alone.

At start the proxy registers a BlueZ agent with the NoInputNoOutput capability and asks to be the default agent.
It confirms "Just Works" pairing and service authorization only for devices connected through the proxy, and
refuses PIN and passkey entry, so devices that need a code cannot be paired this way. If another program holds
the default agent, the proxy keeps its own registered for the pairings it starts and logs a warning. `-agent=false`
skips the agent; BlueZ then still pairs devices that need no confirmation.

Unpairing calls `Adapter1.RemoveDevice`, because BlueZ has no call that removes only the bond: it also forgets
the device's cached services and drops its connection, which the proxy reports to Home Assistant after the
unpairing reply. The device shows up again with its next advertisement.

Alternatively, pair once with `bluetoothctl`, or move an existing bond: stop `bluetoothd` on both machines, copy
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
