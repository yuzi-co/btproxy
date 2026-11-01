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
- Pairing and unpairing on request from Home Assistant ("Just Works" only, see [Bonded devices](#bonded-devices)).
- Active and passive scanning, scanner state and mode reporting, and mode changes from Home Assistant (see
  [Scanning](#scanning)).
- Recovery without a restart when `bluetoothd` restarts, the adapter disappears and returns, or it is powered
  off: connections are reported dropped, then the adapter is powered on, scanning resumes and, after a
  `bluetoothd` restart, the pairing agent is registered again. A `bluetoothd` that hangs is detected too (see
  [Scanning](#scanning)) and, if `-bluetoothd-restart` is set, restarted.
- API encryption (Noise, as ESPHome's `api: encryption: key:`), off by default (see [Encryption](#encryption)).
- Not supported: connection parameter changes, setting the encryption key from Home Assistant, IPv6 mDNS.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

The binaries are static, about 4 MB (4.9 MB for mipsle).

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
`-encryption-key` and `-encryption-key-file` (see [Encryption](#encryption)), `-debug`.

Home Assistant discovers the proxy over mDNS and offers it under Settings, Devices & services. Without mDNS
(`-mdns=false`, or a network that does not pass multicast), add it by hand: Add integration, ESPHome, host and
port 6053.

## mDNS

btproxy advertises itself like an ESPHome device: `_esphomelib._tcp` service `<name>` on host `<name>.local`
(`-name` reduced to a DNS label), with the TXT keys ESPHome's mdns component sends and Home Assistant reads:
`friendly_name`, `version` (the ESPHome version btproxy reports), `mac` (the `-mac` value, lower case without
colons), `platform=linux`, `board` (the Go architecture), `network=ethernet`, `project_name` and
`project_version`. With an encryption key, `api_encryption=Noise_NNpsk0_25519_ChaChaPoly_SHA256` too, which
tells Home Assistant to ask for the key when it discovers the proxy.

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

## Encryption

Without a key the API is plaintext, like an ESPHome node without `api: encryption:`. With a key, the proxy
accepts only encrypted clients: Noise_NNpsk0_25519_ChaChaPoly_SHA256 with the key as the pre-shared key, the
protocol ESPHome uses. The key is the same kind as ESPHome's, 32 random bytes in base64, for example from
`head -c 32 /dev/urandom | base64`. Give it in one of three ways:

- `-encryption-key-file /etc/btproxy.key`: a file holding the key (surrounding whitespace is ignored). Keep it
  readable by root only.
- `-encryption-key <key>`: simplest, but any local user can read it with `ps`.
- the environment variable `BTPROXY_ENCRYPTION_KEY`, used only when neither flag is set.

Setting both flags, or a key that is not base64 of 32 bytes, stops the proxy at start.

The init script and the systemd unit in [contrib](contrib) show how to pass the file. Home Assistant needs the
same key: for a new proxy it asks during setup; for one added before without encryption, the next connection
fails with "requires encryption" and Home Assistant starts a reauthentication that asks for it. A client with
another key is refused with the message ESPHome sends ("Handshake MAC failure"), which Home Assistant reports as
a wrong key, and a plaintext client gets the reply that makes it report that encryption is required. Without a
key, an encrypted client is told the device uses plaintext.

Home Assistant can also offer to set a key on a plaintext ESPHome device (`NoiseEncryptionSetKeyRequest`). The
proxy refuses that, because it does not store a key itself, and does not claim `api_encryption_supported` in
its device info unless it already runs with a key.

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

Passive scanning has limits that active scanning does not:

- BlueZ has no match-all monitor, and 5.66 overflows a buffer for monitors with more than 7 patterns, so the
  proxy registers three: common Flags values, manufacturer data of common company ids (Apple, Microsoft,
  Samsung, Google, Ruuvi, Govee, SwitchBot) and common 16-bit service data (Xiaomi, BTHome, SwitchBot,
  environmental sensing, Google, Tile). Advertisements without Flags that match none of these are not seen.
- Without discovery, BlueZ reports a device only when its advertisement content changes or its RSSI moves by
  8 dB or more, so unchanged beacons arrive far less often and RSSI is coarse. Prefer active scanning for
  presence detection.
- No scan responses, so names and other scan-response data of active-only devices are missing.

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
  Connecting does not depend on discovery: for `Device1.Connect` and `ConnectDevice` the kernel scans for the
  device itself, so connections work in passive mode too.
- There is no idle timeout on API clients: Home Assistant does not ping while advertisements flow, so dead peers
  are found with TCP keepalive (30 s idle, then 3 probes 10 s apart).

## Deploy

OpenWrt (23.05): `opkg install kmod-bluetooth bluez-daemon` plus the firmware for the stick (for example
`rtl8761bu-firmware`), copy the build for the router's architecture (`dist/btproxy-mipsle` for MT7620/MT7621) to
`/usr/bin/btproxy`, and [contrib/openwrt/btproxy.init](contrib/openwrt/btproxy.init) to `/etc/init.d/btproxy`.
For an encrypted API, put the key in `/etc/btproxy.key` (`chmod 600`); the init script passes it when the file
exists. OpenWrt keeps BlueZ's state in `/var/lib/bluetooth` on tmpfs; link it to a directory on flash (for
example `/etc/bluetooth/keys`) so bonds survive a reboot, and list `/usr/bin/btproxy`, `/etc/init.d/btproxy`,
that directory and the key file in `/etc/sysupgrade.conf`.

systemd (Raspberry Pi OS, Debian): copy the binary to `/usr/local/bin/btproxy` and
[contrib/systemd/btproxy.service](contrib/systemd/btproxy.service) to `/etc/systemd/system/`, then
`systemctl enable --now btproxy`. The unit wants `bluetooth.service` rather than requiring it, so a
`bluetoothd` restart leaves the proxy running and it recovers by itself. For an encrypted API, put the key in
`/etc/btproxy.key` (`chmod 600`) and swap `ExecStart` for the two commented lines in the unit, which hand it to
the proxy as a systemd credential.

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
# encrypted: start btproxy with -encryption-key-file k (a test key), then
.venv/bin/python test/e2e.py 127.0.0.1 16053 "$(cat k)"
```
