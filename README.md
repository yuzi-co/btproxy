# btproxy

An ESPHome-compatible Bluetooth proxy for Linux, written in Go. Home Assistant adds it through the ESPHome
integration (port 6053) the same way it adds an ESP32 proxy, then scans and connects to BLE devices through the
host's Bluetooth adapter. It talks to BlueZ over D-Bus, so other programs can use the adapter at the same time.

Status: in daily use since 2026-10-31 on an OpenWrt 23.05 router (MT7620A, mipsle, 128 MB RAM) with a Realtek
RTL8761B USB stick. It serves Home Assistant 2026.11 with advertisements for device tracking and keeps a bonded
connection to a Tion breezer. The fake backend passes an end-to-end test with `aioesphomeapi` (Home Assistant's
client library).

## What it supports

- Raw advertisements, batched like ESPHome: read from the controller's HCI events when the proxy may open the
  kernel's HCI monitor, otherwise rebuilt from BlueZ's parsed properties (see
  [Advertisements](#advertisements)).
- Active connections: connect, service discovery, read and write of characteristics and descriptors,
  notifications, disconnect, connection slot reporting, cache clearing. Up to `-connections` (default 3).
- Pairing and unpairing on request from Home Assistant ("Just Works" only, see [Bonded devices](#bonded-devices)).
- Connection parameter changes (interval, latency, supervision timeout) on a live connection, sent as an HCI
  command (see [Connection parameters](#connection-parameters)).
- Active and passive scanning, scanner state and mode reporting, and mode changes from Home Assistant (see
  [Scanning](#scanning)).
- Recovery without a restart when `bluetoothd` restarts, the adapter disappears and returns, or someone powers
  it off. The proxy reports the connections as dropped, powers the adapter on, resumes scanning and, after a
  `bluetoothd` restart, registers the pairing agent again. It also detects a `bluetoothd` that hangs (see
  [Scanning](#scanning)) and restarts it if `-bluetoothd-restart` is set.
- Log streaming to Home Assistant (the ESPHome entry's option to subscribe to the device's logs) and to
  `esphome logs`, formatted like an ESPHome device's (see [Logs](#logs)).
- API encryption (Noise, as ESPHome's `api: encryption: key:`), off by default (see [Encryption](#encryption)).
- Not supported: setting the encryption key from Home Assistant, IPv6 mDNS.

## Compared with other proxies

As of 2026-11, against ESPHome's `bluetooth_proxy` on an ESP32 and [denvera/bt-proxy](https://github.com/denvera/bt-proxy)
(Python and bleak on Linux, ESPHome API 1.10). "?" means not checked.

| Feature | btproxy | ESPHome ESP32 | denvera/bt-proxy |
|---|---|---|---|
| Runs on | Linux with BlueZ, one static binary (5 MB on mipsle) | ESP32 | Linux with BlueZ, Python |
| Advertisements | true raw from the HCI monitor, rebuilt from BlueZ as fallback | true raw | rebuilt by bleak |
| Passive scanning, scanner state and mode | yes (passive needs `bluetoothd --experimental`) | yes | yes |
| Connections, GATT read/write/notify | yes | yes | yes |
| Pairing and unpairing | yes (Just Works) | yes | no |
| Cache clearing | yes, bonds kept | yes | reported, nothing cleared |
| Connection parameters | yes (HCI LE Connection Update) | yes | no |
| API encryption (Noise) | yes, off by default | yes | no |
| Log streaming to Home Assistant | yes | yes | ? |
| mDNS discovery | yes (IPv4) | yes | yes |
| Recovery from `bluetoothd` restarts and hangs | yes, optional restart command | n/a | restarts yes, hangs ? |
| Hardware cost | a Linux box you already have | an ESP32 board, a few euros | a Linux box you already have |
| Shares the adapter with other programs | yes, through BlueZ | n/a | yes, through BlueZ |

Shelly Gen2+ devices, the Ruuvi Gateway and Theengs / OpenMQTTGateway also relay advertisements to Home
Assistant, but cannot make connections.

An ESP32 is still the better proxy where you have USB power and nothing else: it is cheap, and its radio
handles BLE alone. btproxy makes sense where a Linux machine with a Bluetooth adapter already sits in the
right room. On the router above, the USB stick sits next to the router's own 2.4 GHz radio, and many connection
attempts fail at the radio level (HCI reason 0x3e), not in the software.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

The binaries are static, about 4 to 5 MB (5.0 MB for mipsle).

`-tags nonoise` leaves out API encryption (Noise, ChaCha20-Poly1305, X25519), which makes the mipsle binary
256 KB smaller (4.8 MB). Such a build stops at start with an error when given an encryption key
(`-encryption-key`, `-encryption-key-file` or `$BTPROXY_ENCRYPTION_KEY`). The router build uses it while
encryption is off there:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -tags nonoise -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

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
default `auto`),
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

## Advertisements

With `-raw-adverts auto` (the default) or `on`, the proxy opens the kernel's HCI monitor channel, the read-only
socket `btmon` uses, and takes advertisements from the controller's LE Advertising Report and LE Extended
Advertising Report events. Home Assistant then gets the bytes that were on the air: field order, address type,
AD types BlueZ does not parse, and every report the controller passes up, including repeats BlueZ would have
dropped as unchanged. `bluetoothd` still runs the controller. It starts and stops scanning and makes the
connections, and the monitor only listens. Without the monitor (`-raw-adverts off`, or `auto` when it cannot be
opened), the proxy rebuilds advertisements from BlueZ's parsed properties (see [Notes](#notes)).

The monitor needs `CAP_NET_RAW` (root has it) and a kernel with Bluetooth sockets, which any kernel that runs
BlueZ has. `auto` falls back to BlueZ's properties when the socket cannot be opened, when reading it fails, and
when it delivers nothing while BlueZ reports 30 advertisements. `on` refuses to start without the monitor, but
still falls back on later failures. The log says which source is in use. The proxy reads only the
`-adapter` controller (`hciN` is controller index N), and a socket filter in the kernel passes it only
advertising reports, so connection traffic does not wake it up.

The proxy forwards reports the way an ESP32 proxy does. During active scanning, an ESP32 holds the
advertisement of a scannable device until the scan response arrives, then sends both as one entry with the data
concatenated and the response's RSSI. The proxy does the same, but sends a held advertisement alone if no
response follows within 200 ms or the device advertises again first. Other advertisements go out at once. An
entry carries at most 62 bytes. Longer extended advertisements keep the AD structures that fit, and the proxy
reads only their first fragment. It skips anonymous advertisements and reports without RSSI. It reports
address types as public or random, and for a private address the controller resolved, the identity address's
type.

The controller decides how many advertisements reach the monitor. For active discovery the kernel turns the
controller's duplicate filter on, so it may report a device about once per scan (controllers differ in what
counts as a duplicate). The kernel turns the filter off while an advertisement monitor is registered. So with
the HCI monitor in use, the proxy also registers its advertisement monitors during active scanning, when
`bluetoothd` has the monitor API (`--experimental`, see [Scanning](#scanning)), and asks BlueZ for discovery
without `DuplicateData`, which it no longer needs. Passive scanning has no scan responses either way.

Unlike `bluetoothd`, the monitor does not resolve private addresses. A bonded device that uses resolvable
private addresses shows up under its current random address, not its identity address, unless the controller
resolves it.

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
same key. For a new proxy it asks during setup. For a proxy added earlier without encryption, the next
connection fails with "requires encryption", and Home Assistant starts a reauthentication that asks for the key.

The proxy answers clients the way ESPHome does. It refuses a client with another key with "Handshake MAC
failure", which Home Assistant reports as a wrong key. A plaintext client gets the reply that makes Home
Assistant report that encryption is required. Without a key, the proxy tells an encrypted client that the
device uses plaintext.

Home Assistant can also offer to set a key on a plaintext ESPHome device (`NoiseEncryptionSetKeyRequest`). The
proxy refuses, because it does not store a key itself. It claims `api_encryption_supported` in its device info
only when it already runs with a key.

## Scanning

`-scan-mode` is the configured mode Home Assistant reads back. Home Assistant can switch the mode at run time
(its auto mode keeps a proxy passive and opens short active windows). The state reports carry the mode actually
in use, which is active whenever passive scanning is unavailable. States: running, idle (no subscriber), failed
(`bluetoothd` or the adapter is gone, scanning could not start, `bluetoothd` did not answer, or scanning
stalled; retried every 10 s), stopped (the adapter is powered off and could not be powered on).

`bluetoothd` can hang while keeping its D-Bus name. With BlueZ 5.66 and an RTL8761B, `StartDiscovery` once
never replied, and the proxy sat with no advertisements for hours. Every call the proxy makes that
`bluetoothd` should answer at once now gives up after 10 s, which counts as a failure. GATT operations,
connecting and pairing keep their own 30 s limits. In active mode a
watchdog also stops and restarts discovery when no advertisement at all has arrived for `-stall-timeout`
(3 minutes), reporting failed in between. It does not run in passive mode, where BlueZ reports a device only
when its advertisement changes and long silences are normal. After two stalls or unanswered calls in a row,
with no advertisement in between, the proxy runs the `-bluetoothd-restart` command (through `/bin/sh -c`, at
most once every 10 minutes, killed after 30 s). The usual recovery after a `bluetoothd` restart then brings
scanning back. The init script and the systemd unit in [contrib](contrib) set the command. Without the command, the proxy keeps retrying
and reporting failed.

Active scanning is BlueZ discovery (`StartDiscovery`, LE only, duplicate data on). Passive scanning registers
advertisement monitors (`AdvertisementMonitorManager1`), with which the kernel scans passively and BlueZ
updates the devices that match. This API needs BlueZ 5.56 or later, a kernel with advertisement monitor
support (5.10 or later), and `bluetoothd --experimental` (`-E`). Even current BlueZ registers the monitor
manager only in experimental mode. OpenWrt's `bluez-daemon` init script starts `bluetoothd -n` without it, so
there the proxy falls back to active scanning. To fix that, change the command in `/etc/init.d/bluetoothd` to
`procd_set_param command "$PROG" -n -E` (which also enables `ConnectDevice`) and list the file in
`/etc/sysupgrade.conf`. A `bluez-daemon` package upgrade overwrites the file. After `bluetoothd` restarts, the
proxy tries passive scanning again.

Passive scanning has limits that active scanning does not. The first two apply only to advertisements rebuilt
from BlueZ's properties. The HCI monitor sees every advertisement in range that the controller reports, unless
the controller filters monitor patterns itself (Microsoft's HCI extension, which some Intel and Realtek
controllers have). How often it reports a device depends on the kernel's duplicate filter for passive scans,
which 5.15 turns off while monitors are registered and 6.x leaves on.

- BlueZ has no match-all monitor, and 5.66 overflows a buffer for monitors with more than 7 patterns, so the
  proxy registers three: common Flags values, manufacturer data of common company ids (Apple, Microsoft,
  Samsung, Google, Ruuvi, Govee, SwitchBot) and common 16-bit service data (Xiaomi, BTHome, SwitchBot,
  environmental sensing, Google, Tile). Advertisements without Flags that match none of these are not seen.
- Without discovery, BlueZ reports a device only when its advertisement content changes or its RSSI moves by
  8 dB or more, so unchanged beacons arrive far less often and RSSI is coarse. Prefer active scanning for
  presence detection.
- No scan responses, so names and other scan-response data of active-only devices are missing.

## Connection parameters

Home Assistant integrations can ask for other connection parameters on a connected device (aioesphomeapi's
`bluetooth_device_set_connection_params`, `set_connection_params` in bleak-esphome), for example a shorter
interval during a transfer or a longer one to save the peripheral's battery. BlueZ has no D-Bus call for
this. Its management interface has one (`Load Connection Parameters`), but that only applies to later
connections and replaces the list `bluetoothd` keeps there. So the proxy sends the HCI LE Connection Update
command itself on a raw HCI socket, as `hcitool lecup` does: it looks up the connection handle with the
`HCIGETCONNINFO` ioctl, sends the command and answers once the controller's Command Status arrives, as an
ESP32 does after `esp_ble_gap_update_conn_params`. The completion event, with the parameters the peripheral
accepted, comes some connection events later and is only logged. The kernel keeps owning the connection and
processes the controller's events as if it had sent the command.

Sending HCI commands on a raw socket needs `CAP_NET_RAW` (the init script and the systemd unit run the proxy
as root). Without it, or when the controller refuses the parameters, Home Assistant gets error 0x85; a device
that is not connected through the proxy gets -1, like on an ESP32. The feature is advertised regardless, since
whether the command will be allowed is only known when it is sent.

## Notes

- Without the HCI monitor (see [Advertisements](#advertisements)): BlueZ exposes advertisements only in parsed
  form (name, manufacturer data, service data, UUIDs, flags, TX power), so the raw AD structures are rebuilt
  from those and capped at 62 bytes. Field order differs from the air, which does not matter because Home
  Assistant parses the fields. `UUIDs` can include services BlueZ learnt
  by connecting.
- Attribute handles come from BlueZ's object paths (`service000a/char000b/desc000d`). BlueZ names a
  characteristic after its declaration handle; the proxy reports the value handle after it (`0x000c` here), as an
  ESP32 does, because Home Assistant shares one GATT cache per device between proxies.
- BlueZ owns the CCCD descriptors, so a write to one turns notifications on or off with
  `StartNotify`/`StopNotify` instead.
- Discovery keeps running while the proxy connects. ESPHome pauses it, BlueZ does not need that.
  Connecting does not depend on discovery: for `Device1.Connect` and `ConnectDevice` the kernel scans for the
  device itself, so connections work in passive mode too.
- There is no idle timeout on API clients: Home Assistant does not ping while advertisements flow, so dead peers
  are found with TCP keepalive (30 s idle, then 3 probes 10 s apart).
- Each API client has its own send queue (256 messages) and writer, so a client that stops reading never holds
  up Bluetooth events or other clients. Advertisement batches and log lines are dropped for a client once half
  its queue is waiting; anything else (GATT responses, notifications, connection changes) that finds the queue
  full closes the client, as ESPHome drops a client that cannot keep up, since the stream would no longer be
  consistent. A write that takes over 10 s closes the client too.

## Logs

btproxy logs to stderr (`-debug` for debug lines), which OpenWrt's procd and systemd pass to the system log.
Every line also goes to API clients that subscribe to logs, at the level each one asks for, even below
`-debug`'s threshold: a client subscribed at DEBUG gets debug lines while stderr stays at info. Lines are
formatted like ESPHome's logger, `[I][server:269]: connected addr=AA:BB:CC:DD:EE:01 mtu=247`, in its ANSI
colours, with the Go file and line in place of the component tag; levels map error, warn, info and debug to
ESPHome's ERROR, WARN, INFO and DEBUG. Home Assistant subscribes at the level its ESPHome integration logs at
and writes the lines to its own log.

A subscription with `dump_config` gets one CONFIG line with the proxy's settings (name, versions, MAC, backend,
adapter, scan mode, connections in use). Logging never waits for a client: lines wait in a queue of 64 per
subscriber, then in the client's send queue, and when a slow client lets those fill up, further lines are
dropped for that client and it gets a warning with the count once it catches up.

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
unpairing reply. The device shows up again with its next advertisement. Unpairing a device BlueZ does not
know succeeds, since there is no bond to remove.

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
