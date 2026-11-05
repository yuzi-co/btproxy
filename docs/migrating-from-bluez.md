# Moving from BlueZ to the kernel backend

BlueZ support in btproxy is deprecated. The kernel backend does the same job without `bluetoothd` or D-Bus. This guide is for a host that runs btproxy on BlueZ today.

## Check where you are

Look for `backend chosen` in the log:

```sh
logread | grep 'backend chosen'                # OpenWrt
journalctl -u btproxy | grep 'backend chosen'  # systemd
```

`backend=kernel` means there is nothing to do. On `backend=bluez`, the `reason` says why. Releases before 0.5 do not log this line, and they always use BlueZ.

## Can this host move?

The kernel backend needs the adapter to itself. While `bluetoothd` runs, `auto` picks BlueZ and `-backend kernel` refuses to start. If this host needs `bluetoothd` for something else, such as headphones, it cannot move. Stay on BlueZ for now. Once BlueZ support goes, stay on the last release that has it, or use an ESP32 or another machine as the proxy.

## Check your bonds first

The first start on the kernel backend copies bonds from `/var/lib/bluetooth`, so they must still be there. This lists the devices BlueZ has for each adapter:

```sh
ls /var/lib/bluetooth/*/
```

On OpenWrt that directory is on tmpfs unless you linked it to flash, so without the link it starts empty after every reboot, and devices bonded before that reboot need pairing again.

Only the first start that finds BlueZ bonds for an adapter copies them. Once the kernel backend has a bond file for the adapter, later BlueZ bonds are not copied.

## OpenWrt: upgrading from 0.4 may move you

The package no longer depends on BlueZ. If `bluez-daemon` came in as this package's dependency, the upgrade removes it, and the router is on the kernel backend when the proxy starts again. You do not need the steps under [Move](#move). Go to [Check it worked](#check-it-worked).

A `bluez-daemon` you installed by name stays, and so does the use of BlueZ.

To stay on BlueZ through the upgrade:

- apk: run `apk add bluez-daemon` before the upgrade.
- opkg: after the upgrade, run `opkg install bluez-daemon`, `/etc/init.d/bluetoothd enable`, `/etc/init.d/bluetoothd start` and `/etc/init.d/btproxy restart`.

The package upgrade itself has only run in containers with no Bluetooth adapter, on OpenWrt 23.05, 24.10 and 25.12. They check that the upgrade leaves `/var/lib/bluetooth` in place. Copying real bonds has been tried by stopping `bluetoothd` by hand, not straight after a package upgrade.

## Move

For a host that keeps `bluetoothd` installed: any systemd or Docker host, and a router with BlueZ installed by name. Upgrade to the latest release first, and keep BlueZ installed until the move has worked, so that you can go back quickly.

OpenWrt:

```sh
/etc/init.d/bluetoothd stop
/etc/init.d/bluetoothd disable
/etc/init.d/btproxy restart
```

systemd:

```sh
systemctl mask --now bluetooth
systemctl restart btproxy
```

Mask rather than disable: another unit or a D-Bus request can start a service that is only disabled.

Docker: stop `bluetoothd` on the host, mount `/var/lib/bluetooth:/var/lib/bluetooth:ro` for the first start, and restart the container. The Docker image has not been tried against a real adapter yet.

## Check it worked

- The log says `backend chosen backend=kernel`.
- Home Assistant shows the proxy as connected, and its advertisements keep arriving.
- Connect to each bonded device from Home Assistant.

## What changes

| | BlueZ | Kernel |
|---|---|---|
| Bonds | `/var/lib/bluetooth` | `-bond-dir`: `/etc/btproxy` on OpenWrt, `/var/lib/btproxy` under the systemd unit |
| `-raw-adverts` | used | ignored: it always reads the HCI monitor |
| `-bluetoothd-restart` and the Restart Bluetooth stack button | used | do nothing |
| `-agent` | used | not used: the backend confirms Just Works pairing itself |
| Pairing on demand | not tested | a device that refuses a read or write for lack of encryption gets paired, with Just Works |

The radio stays the same, and so do its failures. The kernel backend did not reduce HCI 0x3e failures on the test router. See [#26](https://github.com/yuzi-co/btproxy/issues/26).

If a bonded device fails with HCI 0x3e on every connect, check that it is bonded to this adapter's address. A Tion Breezer 4S ignored every other adapter until it was put in pairing mode. See [Bonded devices](../README.md#bonded-devices).

## Remove BlueZ

If BlueZ is still installed and you are happy with the move, this frees about 3 MB on OpenWrt:

```sh
apk del bluez-daemon      # 25.12 and newer
opkg remove bluez-daemon  # 24.10 and older, followed by dbus and the libraries it leaves behind
```

On other distributions, remove the `bluez` package if nothing else needs it.

The `-nobluez` archives leave BlueZ and D-Bus out of the binary as well, which saves about 0.6 to 0.7 MiB. On an OpenWrt router that is only about 0.2 MB of flash, because the overlay compresses the binary. A `nobluez` build always uses the kernel backend.

## Go back

1. If BlueZ is gone, install it: `apk add bluez-daemon` or `opkg install bluez-daemon`. A `nobluez` build also has to be replaced with a regular one.
2. OpenWrt: `/etc/init.d/bluetoothd enable; /etc/init.d/bluetoothd start`. systemd: `systemctl unmask bluetooth; systemctl enable --now bluetooth`.
3. Restart the proxy. `auto` picks BlueZ while `bluetoothd` runs. If you set `-backend kernel`, change it to `auto` or `bluez` first.

Devices paired since the move need pairing again, because BlueZ does not have their keys.
