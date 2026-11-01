# btproxy

An ESPHome-compatible Bluetooth proxy for Linux and Windows, written in Go. Home Assistant adds it through the
ESPHome integration (port 6053) like an ESP32 proxy, then scans and connects to BLE devices through the host's
adapter: BlueZ over D-Bus on Linux, WinRT on Windows. Other programs can use the adapter at the same time.

Not related to [conorpp/btproxy](https://github.com/conorpp/btproxy), a Bluetooth man-in-the-middle tool.

Status: in use since 2026-10-31 on an OpenWrt 23.05 router (MT7620A, mipsle, 128 MB RAM, RTL8761B USB stick)
and since 2026-11-01 on Windows 11 (Intel adapter), with Home Assistant 2026.11. The fake backend passes an
end-to-end test with `aioesphomeapi`.

## Features

- Advertisements batched like ESPHome, with an optional [filter](#advertisement-filter).
- Connections with GATT read, write and notify, slot reporting and cache clearing; radio-level connect failures
  are retried and scanning pauses while connecting, as on an ESP32 ([Scanning](#scanning)).
- Pairing and unpairing from Home Assistant ([Bonded devices](#bonded-devices)).
- [Connection parameter](#connection-parameters) changes: exact on Linux, presets on Windows 11.
- Active and passive scanning, mode changes from Home Assistant.
- [Log streaming](#logs), diagnostic [sensors](#sensors), optional [encryption](#encryption).
- [mDNS](#mdns) discovery that skips container, VM and VPN interfaces.
- A settings file and an optional password-protected [settings page](#settings-page).
- Linux: raw advertisements from the HCI monitor, [several adapters](#several-adapters), recovery from
  `bluetoothd` restarts, hangs and adapter loss.
- Windows: tray icon, Start with Windows, log file ([Windows](#windows)).

Not supported: setting the encryption key from Home Assistant, IPv6 mDNS, running as a Windows service.

## Compared with other proxies

As of 2026-11, against ESPHome's `bluetooth_proxy` on an ESP32 and
[denvera/bt-proxy](https://github.com/denvera/bt-proxy) (Python and bleak, ESPHome API 1.10). All three make
GATT connections and discover over mDNS. "?" means not checked.

| Feature | btproxy | ESPHome ESP32 | denvera/bt-proxy |
|---|---|---|---|
| Runs on | Linux (BlueZ), Windows 10 2004+ and 11; static binary, about 4 MB | ESP32 | Linux (BlueZ), Python |
| Advertisements | Linux: raw from HCI monitor, else rebuilt; Windows: rebuilt | raw | rebuilt by bleak |
| Passive scanning | Linux needs `bluetoothd --experimental` | yes | yes |
| Retry of radio connect failures | yes | ? | yes |
| Pairing, unpairing | yes (Just Works) | yes | no |
| Cache clearing | yes, bonds kept | yes | reported, nothing cleared |
| Connection parameters | Linux: exact; Windows 11: nearest preset | yes | no |
| API encryption | yes, off by default | yes | no |
| Diagnostic sensors | proxy statistics | general ones if added in YAML | no |
| Advertisement filter | allow/deny lists | ? | no |
| Several adapters | Linux: one device each | n/a | one per process |
| Settings | flags, file, web page; Windows tray | YAML, compiled in | flags |
| Recovers from `bluetoothd` restarts, hangs | yes | n/a | restarts ?; on a hang it backs off and logs a hint |

Shelly Gen2+, the Ruuvi Gateway and Theengs / OpenMQTTGateway relay advertisements but cannot connect.

An ESP32 is the better proxy where you only have USB power: cheap, and its radio does BLE alone. btproxy fits
where a Linux box or Windows PC with an adapter already sits in the right room. A USB stick next to a router's
2.4 GHz radio can do badly: on the router above, many connects fail at the radio level (HCI reason 0x3e).

## Install

Download a binary from the [releases](https://github.com/yuzi-co/btproxy/releases), then see [Deploy](#deploy).
With Go 1.27.3 or newer: `go install github.com/yuzi-co/btproxy/cmd/btproxy@latest`.

## Build

Needs Go 1.27.3 or newer.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64  go build -trimpath -ldflags="-s -w" -o dist/btproxy-arm64  ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o dist/btproxy-armv7 ./cmd/btproxy
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -H windowsgui" -o dist/btproxy.exe ./cmd/btproxy
```

Sizes as of 2026-11-01: arm64 and armv7 3.6 MiB, amd64 3.8 MiB, mipsle 4.1 MiB, Windows 3.7 MiB. Without
`-H windowsgui` the Windows binary is a console program that logs like on Linux, with no tray.

`-tags nonoise` leaves out API encryption (192 KiB less on mipsle, 3.9 MiB); such a build refuses to start when
given a key:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build -tags nonoise -trimpath -ldflags="-s -w" -o dist/btproxy-mipsle ./cmd/btproxy
```

Generated code: `go generate ./internal/ble/winrt/gen` (WinRT bindings), `go generate ./cmd/btproxy` (icon).

## Run

Linux needs `bluetoothd`, the system D-Bus and permission to use the adapter (root, or a D-Bus policy); raw
advertisements and connection parameters need `CAP_NET_RAW`. Connecting to a device BlueZ has not seen uses
`ConnectDevice`, which needs `bluetoothd --experimental` (devices in range are normally known from scanning). In
a container it needs the host network (Bluetooth sockets only work in the initial network namespace) and the
host's D-Bus socket. Windows needs only the binary and an LE adapter.

```sh
btproxy -name btproxy-bedroom -area Bedroom
```

Home Assistant discovers it over mDNS. Otherwise add it by hand: Add integration, ESPHome, host, port 6053.

| Flag | Default | Meaning |
|---|---|---|
| `-name` | `btproxy-<hostname>` | node name |
| `-friendly-name` | the name | |
| `-area` | | suggested area |
| `-listen` | `:6053` | API address; see [several adapters](#several-adapters) |
| `-backend` | `bluez`, `winrt` on Windows | `bluez`, `winrt` or `fake` |
| `-adapter` | `hci0` | adapter, or comma-separated list (bluez) |
| `-connections` | 3 | connection slots per adapter |
| `-mac` | first interface MAC that is neither locally administered nor a hypervisor's | unique id Home Assistant keys the device on |
| `-mdns` | `true` | advertise over mDNS |
| `-mdns-iface` | all up IPv4 interfaces except container, VM, VPN | comma-separated, see [mDNS](#mdns) |
| `-scan-mode` | `active` | `active` or `passive` |
| `-raw-adverts` | `auto` | `auto`, `on`, `off`: use the HCI monitor (bluez), see [Advertisements](#advertisements) |
| `-scan-pause-on-connect` | `true` | stop scanning while connecting |
| `-connect-retries` | 2 | retries of a connect that fails at once on the radio; `0` disables |
| `-stall-timeout` | `3m` | restart active scanning after this long without advertisements; `0` disables (bluez) |
| `-bluetoothd-restart` | | command run when `bluetoothd` hangs, such as `"/etc/init.d/bluetoothd restart"` or `"systemctl restart bluetooth"` |
| `-agent` | `true` | register a BlueZ pairing agent |
| `-adv-allow`, `-adv-deny`, `-adv-deny-random` | | see [Advertisement filter](#advertisement-filter) |
| `-encryption-key`, `-encryption-key-file` | | see [Encryption](#encryption); also `$BTPROXY_ENCRYPTION_KEY` |
| `-config` | Windows: `%LOCALAPPDATA%\btproxy\config.json` | settings file |
| `-ui` | Windows: 127.0.0.1, free port | settings page address; needs a password, see [Settings page](#settings-page) |
| `-ui-password`, `-ui-password-file` | | settings page password; also `$BTPROXY_UI_PASSWORD` |
| `-debug` | `false` | debug logging |
| `-tray` | on when there is no console | Windows tray icon |
| `-log-file` | | also log to this file (Windows) |

### Several adapters

`-adapter hci0,hci1` serves each adapter as its own ESPHome device (Home Assistant counts each as one scanner
with its own slots). The first is served as if alone. Each further one gets `-<adapter>` appended to the name
(`btproxy-gw-hci1`), its Bluetooth address as unique id, the next port (6054, ...) and its own mDNS record, so
the first one's Home Assistant entry is unaffected. `-listen :6053,:7053` sets each address. `-connections`,
`-scan-mode`, `-area` and the key apply to all.

One pairing agent serves all adapters; `-bluetoothd-restart` runs at most once per 10 minutes for all. A
further adapter that cannot be opened at start is logged and skipped (restart the proxy once it is back); the
first one failing stops the proxy. Log lines are tagged with `node` and `adapter`.

## Settings page

`-config FILE` is a flat JSON object of flag values, such as `{"friendly-name": "Garage", "connections": 3}`,
read at start. Command-line flags override it; a bad entry is logged and the rest apply. A file that cannot be
read or parsed stops the proxy, since it may hold the encryption key.

`-ui ADDRESS` (such as `:8080`) serves the status (clients, connections, advertisement rate, scanner state, last
warning or error) and a form for the settings relevant to the backend. Settings given on the command line show
as locked; `-bluetoothd-restart` is never editable there. Saving validates, writes only non-default values and,
if anything changed, restarts the proxy (Linux: `exec` with the same PID, invisible to procd or systemd;
Windows: a new process).

- A password is required, or btproxy refuses to start: `-ui-password-file` (preferred), `$BTPROXY_UI_PASSWORD`
  or `-ui-password`. HTTP Basic, any user name, one-second delay after a wrong one. On Windows, a page on a
  loopback address can use a [login link](#windows) from the tray instead; it then answers only Host `localhost`
  or loopback (against DNS rebinding).
- The stored encryption key is never sent to the browser. Leave the field empty to keep it, type or generate a
  new one, or tick Remove; to read it, open the settings file.
- Per-process form token, cross-origin posts refused, nothing external. The Content-Security-Policy blocks
  other origins but allows the page's inline script and style.
- Plain HTTP: on an untrusted network keep it on 127.0.0.1 and use SSH (`ssh -L 8080:127.0.0.1:8080 router`).

The server is a minimal HTTP/1.1 implementation (`net/http` would add about 4 MB; this adds 64 KB, with or
without `-ui`): one request per connection, 16 KB body, 10 s per exchange, 8 connections.

## Windows

Windows 11 (tested) and Windows 10 2004+ (untested), pure Go over WinRT. Uses the default adapter, which must support LE and the central
role; `-adapter`, `-agent`, `-stall-timeout`, `-bluetoothd-restart` and `-raw-adverts` have no effect.

- Tray menu: status, Start with Windows, Settings..., Open log, Quit. Quit, logoff and shutdown withdraw mDNS.
- Start with Windows writes `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\btproxy` without flags, so
  settings go in the settings file.
- The settings page is on by default (127.0.0.1, free port; `-ui ""` turns it off). The tray opens it with a
  random URL token that becomes an HttpOnly, SameSite=Strict cookie, so other local users and sites cannot
  use it. With `-tray=false` the link is printed to the console only, not to the log.
- The settings page saves the settings file with access for its owner and SYSTEM only. A file written by hand
  gets that on the first save; until then it has its directory's permissions.
- Without a console, logs go to `%LOCALAPPDATA%\btproxy\btproxy.log` (rotated at 1 MB to `btproxy.log.1`,
  crash traces included); start errors show a message box.
- One instance per listen address; a second one says so and exits.
- Home Assistant needs TCP 6053 in and, for discovery, UDP 5353; Windows Firewall asks on first start.
- On a laptop that switches between Ethernet and Wi-Fi, set a fixed `-mac`.

Tested on Windows 11 (Intel): scanning, mode changes, connecting, discovery, reads, notification on/off, MTU
247, cache clearing, disconnect. Not yet tried against a device: writes, notification delivery, pairing,
unpairing a bonded device, link loss, radio off and on.

Differences from BlueZ:

- Advertisements are rebuilt from WinRT's parsed sections in original order, so bytes match the air. Scan
  responses arrive as separate entries, as from an ESP32. Extended advertisements are included; PHYs are not
  reported.
- Windows owns connections: the link drops a few seconds after the proxy lets go, and stays up while another
  program or Windows uses the device. Services Windows keeps (such as HID) have no characteristics.
- Windows connects only to devices it has seen advertising or paired ("device not found" right after a start).
- Pairing accepts Just Works, passkey display and numeric comparison; passkey entry fails. Windows keeps the
  bond.
- Connect retries: an out-of-range device hits the 30 s limit (not retried). A link that fails in its first
  connection events (Unreachable, `ERROR_GEN_FAILURE`, `ERROR_DEVICE_NOT_AVAILABLE`, or down after discovery)
  is retried after 500 ms, unless the attempt took over 5 s or the retry would start over 8 s after the first.
  Not found, access denied, cancelled, ATT errors and timeouts fail at once.
- Other programs' watchers keep the radio scanning during the scan pause. A stopped watcher is restarted
  (checked every 10 s, retried every 5 s).

Running as a Windows service is not implemented; see the [design notes](https://github.com/yuzi-co/btproxy/issues/15).

## mDNS

btproxy advertises `_esphomelib._tcp` as `<name>.local` with ESPHome's TXT keys, plus `api_encryption` when it
has a key, so Home Assistant asks for it. Home Assistant uses `mac` as the unique id: an entry added by IP is
matched, not duplicated, and a new address is picked up once the connection is lost.

The responder is built in, IPv4 only. It announces at start and on address changes (checked every 30 s), sends
goodbyes on SIGINT/SIGTERM, answers with the querying interface's address and ignores other subnets. On a router
with VLANs use `-mdns-iface br-lan`. Without it, these are skipped (logged; if none would remain, all are
served):

- Windows: VPN and tunnel adapters by driver (TAP-Windows, OpenVPN, WireGuard, Wintun, Tailscale, ZeroTier,
  AnyConnect, GlobalProtect, Fortinet) or with no MAC. Hypervisor and container adapters (Hyper-V, VirtualBox,
  VMware, Docker, WSL), hypervisor MAC prefixes and locally administered MACs only without a default gateway.
- Linux: by name: `veth*`, `docker*`, `br-<12 hex>`, `podman*`, `cni*`, `flannel*`, `cali*`, `virbr*`, `vnet*`,
  `lxcbr*`, `lxdbr*`, `tun*`, `tap*`.

Port 5353 is shared with `umdns` and `avahi-daemon` (`SO_REUSEADDR`, `SO_REUSEPORT`); unicast to 5353 reaches
only one socket. Keep `-name` different from the host name, which `umdns` already answers for.

## Advertisements

With `-raw-adverts auto` (default) or `on`, advertising reports come from the kernel's HCI monitor (read-only,
like `btmon`, filtered to the `-adapter` controller), so Home Assistant gets the bytes from the air, including
AD types BlueZ does not parse and repeats it would drop. `auto` falls back to rebuilding from BlueZ's properties
when the socket cannot be opened, reading fails, or it is silent while BlueZ reports 30 advertisements; `on`
refuses to start without it. The log names the source. Rebuilt advertisements have a different field order and
`UUIDs` can include services learnt by connecting.

While the monitor works, the proxy ignores BlueZ device signals except for connected devices, and with
`--experimental` has `bluetoothd` stop reporting devices (unless the controller matches monitor patterns
itself), to save CPU. A fallback undoes both.

As on an ESP32, a scannable device's advertisement is held for its scan response and sent with it as one entry
(the response's RSSI), or alone after 200 ms or a new advertisement. Entries are capped at 62 bytes (whole AD
structures; first fragment of extended advertisements). Anonymous advertisements and reports without RSSI are
skipped.

With active discovery the controller's duplicate filter may report a device about once per scan. With the HCI
monitor the proxy also registers advertisement monitors during active scanning (needs `--experimental`), which
turns the filter off. The monitor does not resolve private addresses: a bonded device using them shows under its
current random address unless the controller resolves it.

### Advertisement filter

`-adv-allow` and `-adv-deny` take comma-separated entries: an address (`AA:BB:CC:DD:EE:FF`), an OUI
(`58:2B:DB`) or prefix (`58:2B:*`), a service UUID in UUID lists or service data (`uuid:fe95`, 32-bit or
36-character), or a manufacturer company id (`mfr:0x004c`, `mfr:76`). Deny wins; an empty allow list allows all.
`-adv-deny-random` drops private random addresses (random static and controller-resolved stay): most phones and
trackers, but also phone presence (iBeacon apps, Private BLE Device).

Only forwarding is filtered, not scan watchdogs or connections. Home Assistant connects through a proxy that
heard the device, so do not filter out devices that should connect through this one, such as bonded ones.

## Encryption

Without a key the API is plaintext and has no authentication: any host that can reach the port can use the
proxy, including bonded devices, unpair devices and read its logs. The proxy logs a warning at start unless it
listens on loopback only. With a key, only Noise (Noise_NNpsk0_25519_ChaChaPoly_SHA256, as ESPHome) clients are
accepted. The key is 32 random bytes in base64 (`head -c 32 /dev/urandom | base64`):

- `-encryption-key-file /etc/btproxy.key`: preferred; make it root-readable only
  (`(umask 077; head -c 32 /dev/urandom | base64 > /etc/btproxy.key)`).
- `-encryption-key <key>`: visible to local users in `ps`.
- `$BTPROXY_ENCRYPTION_KEY`: used only when neither flag is set.

Both flags, or an invalid key, stop the proxy at start. See [contrib](contrib) for the init script and unit.
Home Assistant asks for the key during setup, or on reauthentication for a proxy added without one. A wrong key
shows as "Handshake MAC failure". The proxy refuses Home Assistant's offer to set a key, as it does not store
one.

## Scanning

Home Assistant can switch `-scan-mode` at run time. Reported states carry the mode in use (active when passive
is unavailable): running, idle (no subscriber or paused for a connect), failed (retried every 10 s), stopped
(adapter off and cannot be powered on).

Passive scanning uses BlueZ advertisement monitors: BlueZ 5.56+, kernel 5.10+ and `bluetoothd --experimental`
(`-E`); otherwise the proxy scans actively and retries passive after a `bluetoothd` restart. OpenWrt's
`bluez-daemon` starts `bluetoothd -n`: change `/etc/init.d/bluetoothd` to
`procd_set_param command "$PROG" -n -E` (also enables `ConnectDevice`) and list it in `/etc/sysupgrade.conf`;
a package upgrade overwrites it.

Passive scanning has no scan responses. Without the HCI monitor it also:

- sees only advertisements matching three monitors (BlueZ 5.66 overflows with over 7 patterns): common Flags,
  common manufacturer ids (Apple, Microsoft, Samsung, Google, Ruuvi, Govee, SwitchBot) and common service data
  (Xiaomi, BTHome, SwitchBot, environmental sensing, Google, Tile);
- reports a device only when its content changes or RSSI moves 8 dB, so prefer active for presence.

With the monitor it sees every report, unless the controller filters monitor patterns itself (Microsoft HCI
extension on some Intel and Realtek). Kernel 5.15 turns the duplicate filter off for passive scans, 6.x does not.

### Hangs and recovery

`bluetoothd` can hang while keeping its D-Bus name (BlueZ 5.66 with an RTL8761B: several times a night). The
proxy detects it by: D-Bus calls timing out after 10 s (disconnects after 30 s; GATT, connect and pair have their
own 30 s limits), a health-check property read every minute, and in active mode no advertisement for
`-stall-timeout` (which restarts discovery).

After two stalls or timeouts, each within 10 minutes of the previous (and, without the HCI monitor, no
advertisement in between), it runs `-bluetoothd-restart` via `/bin/sh -c` (at most every 10 minutes, killed
after 30 s); the contrib scripts set it. Without it the proxy keeps retrying and reporting failed. After a
restart, adapter loss or power-off, it reports connections dropped, powers the adapter on, resumes scanning and
registers the agent again.

### Scan pause and connect retries

With `-scan-pause-on-connect`, from the connect call until services are resolved or it fails, the proxy stops
scanning and reports idle. Otherwise the kernel resumes scanning once the link is up; with an RTL8761B about
half the connects then failed with HCI reason 0x3e. Overlapping connects share one pause, capped at 60 s. It
cannot stop scanning the kernel does for another pending connect or a `bluetoothd` auto-reconnect. Pairing does
not pause scanning. Windows pauses the same way.

Connects that still fail within about half a second (`le-connection-abort-by-local`, or an error without
message) are retried after 200 ms, still paused, up to `-connect-retries` times. Not retried: failures after
3 s, other errors, withdrawn connects, and retries more than 5 s after the first attempt. A retried connect logs
`connected after retries`.

## Connection parameters

Integrations can request other parameters on a live connection (aioesphomeapi
`bluetooth_device_set_connection_params`).

- Linux: the proxy sends HCI LE Connection Update on a raw socket (like `hcitool lecup`) and answers on Command
  Status. Needs `CAP_NET_RAW`; without it, or if the controller refuses, Home Assistant gets error 0x85, and a
  device not connected through the proxy gets -1.
- Windows 11 (build 22000+): one of three presets, reported by build 26300 as ThroughputOptimized 15 ms,
  Balanced 30 to 60 ms, PowerOptimized 90 to 180 ms (latency 0, timeouts 2, 4, 6 s). The proxy picks the preset
  overlapping the request, else the nearest on a log scale. It holds until the next request or disconnect, and
  Windows weighs it against other apps. Not offered on Windows 10.

## Notes

- Characteristic handles are reported as value handles, as on an ESP32, so Home Assistant's GATT cache works
  across proxies.
- CCCD writes turn notifications on and off through BlueZ or Windows; cache clearing keeps bonds.
- Connecting does not depend on the proxy's scan, so it works in passive mode and during the pause.
- At most 16 API clients; further connections are closed. A client has 30 s to finish the handshake and send
  Hello. After that, dead clients are found with TCP keepalive (30 s idle, 3 probes 10 s apart); there is no idle
  timeout.
- Each client has a 256-message send queue. Advertisements and log lines are dropped once it is half full;
  anything else finding it full closes the client, as does a write over 10 s.
- GATT requests wait in a queue per connection, of at most 256 requests and 1 MiB of write data; one over that
  fails with a GATT error. A client with 32 device or connection parameter requests running is closed.

## Logs

Logs go to stderr (`-debug` for debug lines), or a file on Windows without a console. API clients that subscribe
get every line at the level they ask for, even below `-debug`, formatted like ESPHome's logger with the Go file
and line as tag: `[I][server:269]: connected addr=AA:BB:CC:DD:EE:01 mtu=247`. `dump_config` adds one CONFIG
line with the settings. A slow client misses lines and then gets a warning with the count.

## Sensors

Diagnostic entities, sent on subscribe, every 60 s and on each connection made, ended or failed. Counters reset
at start. Entity keys are hashes of the object ids, stable across versions.

| Object id | Unit | What |
|---|---|---|
| `uptime` | s | time since the proxy started |
| `connection_attempts` | | connects handed to the Bluetooth stack (not those refused for want of a free slot) |
| `connections_made` | | of which connected |
| `connection_success_rate` | % | share of the last 20 attempts that connected; unknown before the first |
| `advertisement_rate` | adv/s | advertisements received, averaged over the last 60 s |
| `advertisements_dropped` | | advertisements not sent to a client that fell behind, counted per client |
| `active_connections` | | established connections |
| `free_connection_slots` | | of `-connections` |
| `bluetoothd_restarts` | | restarts by the watchdog (`-bluetoothd-restart`); bluez backend only |
| `memory` | MiB | resident memory, from `/proc/self/statm`; Linux only |

## Deploy

OpenWrt 23.05:

- `opkg install kmod-bluetooth bluez-daemon` and the stick's firmware (such as `rtl8761bu-firmware`).
- Copy the build (`dist/btproxy-mipsle` for MT7620/MT7621) to `/usr/bin/btproxy` and
  [contrib/openwrt/btproxy.init](contrib/openwrt/btproxy.init) to `/etc/init.d/btproxy`.
- Encryption: key in `/etc/btproxy.key` (`chmod 600`); the init script passes it when present.
- `/var/lib/bluetooth` is on tmpfs: link it to flash (such as `/etc/bluetooth/keys`) so bonds survive a reboot.
- Settings page: the init script passes `-config /etc/btproxy.json`. Set `UI` in it (such as `UI=:8080`), put a
  password in `/etc/btproxy.uipass` (`chmod 600`), set `NAME`, `FRIENDLY_NAME` and `AREA` to `""` to make them
  editable, then `/etc/init.d/btproxy restart`. An older init script without `-config` makes the page read-only.
- List the binary, init script, bonds directory, key, `/etc/btproxy.json` and `/etc/btproxy.uipass` in
  `/etc/sysupgrade.conf`.

systemd (Raspberry Pi OS, Debian): copy the binary to `/usr/local/bin/btproxy` and
[contrib/systemd/btproxy.service](contrib/systemd/btproxy.service) to `/etc/systemd/system/`, then
`systemctl enable --now btproxy`. The unit wants `bluetooth.service` without requiring it, so the proxy survives
a `bluetoothd` restart. For encryption, put the key in `/etc/btproxy.key` (`chmod 600`) and use the two
commented `ExecStart` lines, which pass it as a systemd credential. The unit runs as root with a read-only file
system and other sandboxing; a settings file the page should save needs its directory in `ReadWritePaths=`.

Windows: put the `-H windowsgui` build somewhere permanent, start it, allow it through the firewall (program
rule, or TCP 6053 and UDP 5353), tick Start with Windows in the tray, and configure on the settings page.

### Bonded devices

Devices that need an encrypted link (such as a Tion breezer) must be bonded with the proxy's adapter. Home
Assistant can pair a connected device through the proxy (`bluetooth_device_pair`); an already paired device is
reported paired. The proxy never pairs on its own.

On Linux the proxy registers a NoInputNoOutput agent and asks to be the default. It confirms Just Works pairing
and service authorization only for devices connected through it and refuses PIN and passkey entry. If another
program is the default agent, the proxy uses its own for the pairings it starts and logs a warning.
`-agent=false` skips it; BlueZ still pairs devices that need no confirmation. Unpairing calls
`Adapter1.RemoveDevice`, which also drops the connection and cached services; unpairing an unknown device
succeeds.

To pair outside Home Assistant, use `bluetoothctl`, or move a bond: stop `bluetoothd` on both machines (it
rewrites `info` on exit), copy `/var/lib/bluetooth/<adapter MAC>/<device MAC>/` to the new adapter's directory,
start `bluetoothd`. Bonds belong to an adapter address, so this only works for the same adapter (such as the same
stick in another machine). A device can be bonded to several adapters, each paired while it is in pairing mode.

## Test

```sh
go test ./...                       # the bluez and mDNS socket tests need GOOS=linux (or a Linux container)
go run ./cmd/btproxy -backend fake -listen 127.0.0.1:16053 &
python -m venv .venv && .venv/bin/pip install aioesphomeapi
.venv/bin/python test/e2e.py 127.0.0.1 16053
# encrypted: start btproxy with -encryption-key-file k (a test key), then
.venv/bin/python test/e2e.py 127.0.0.1 16053 "$(cat k)"
```

Profiling: build with `-tags pprof` and without `-s -w`. The proxy serves Go's profiler on `127.0.0.1:6060`;
reach it over an SSH tunnel (`ssh -L 16060:127.0.0.1:6060 root@router`) and run
`go tool pprof -seconds 120 http://127.0.0.1:16060/debug/pprof/profile`
with the unstripped binary.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). The API definitions in [proto](proto) come from ESPHome, also under
[MIT](proto/LICENSE).
