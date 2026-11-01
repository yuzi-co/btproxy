"""End-to-end check of btproxy with aioesphomeapi, the client library Home Assistant uses.

Run btproxy with -backend fake, then: python test/e2e.py [host] [port]
"""

import asyncio
import inspect
import sys

from aioesphomeapi import APIClient, BluetoothScannerMode, BluetoothScannerState

FAKE_ADDR = 0xAABBCCDDEE01
BATTERY, ECHO, CCCD = 0x0003, 0x0013, 0x0014
NUS_SERVICE = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"


async def maybe_await(v):
    return await v if inspect.isawaitable(v) else v


async def main(host: str, port: int) -> None:
    cli = APIClient(host, port, None)
    await cli.connect(login=True)
    info = await cli.device_info()
    caps = await cli.device_capabilities()
    print("capabilities:", caps)
    print("device:", info.name, info.mac_address, "bt", info.bluetooth_mac_address,
          "flags", info.bluetooth_proxy_feature_flags)
    assert info.bluetooth_proxy_feature_flags & 0b10, "active connections not advertised"
    assert info.bluetooth_proxy_feature_flags & 0b1000, "pairing not advertised"

    entities, services = await cli.list_entities_services()
    assert entities == [] and services == []

    scanner = []  # registered first: the state report answers the advertisement subscription
    unsub_scanner = cli.subscribe_bluetooth_scanner_state(
        lambda s: scanner.append((s.state, s.mode, s.configured_mode)))
    adverts = []
    unsub = cli.subscribe_bluetooth_le_raw_advertisements(lambda r: adverts.extend(r.advertisements))
    free = []
    unsub_free = cli.subscribe_bluetooth_connections_free(lambda f, l, a: free.append((f, l, list(a))))
    await asyncio.sleep(1.5)
    assert any(a.address == FAKE_ADDR and a.rssi == -60 for a in adverts), adverts
    print("adverts:", len(adverts), "first data", bytes(adverts[0].data).hex())
    assert free and free[0][:2] == (3, 3), free

    # Started with the default -scan-mode active; Home Assistant may switch modes at run time, while the
    # configured mode stays.
    run, act, pas = BluetoothScannerState.RUNNING, BluetoothScannerMode.ACTIVE, BluetoothScannerMode.PASSIVE
    assert scanner and scanner[-1] == (run, act, act), scanner
    cli.bluetooth_scanner_set_mode(pas)
    await asyncio.sleep(0.3)
    assert scanner[-1] == (run, pas, act), scanner
    cli.bluetooth_scanner_set_mode(act)
    await asyncio.sleep(0.3)
    assert scanner[-1] == (run, act, act), scanner
    print("scanner states:", [(s.name, m.name, c.name) for s, m, c in scanner])

    states = []

    async def connect() -> None:
        connected = asyncio.get_running_loop().create_future()

        def on_state(conn: bool, mtu: int, err: int) -> None:
            states.append((conn, mtu, err))
            if not connected.done():
                connected.set_result((conn, mtu, err))

        await maybe_await(cli.bluetooth_device_connect(
            FAKE_ADDR, on_state, feature_flags=info.bluetooth_proxy_feature_flags, address_type=1))
        print("connect:", await asyncio.wait_for(connected, 5))

    await connect()

    svcs = await cli.bluetooth_gatt_get_services(FAKE_ADDR)
    uuids = [s.uuid for s in svcs.services]
    print("services:", uuids)
    assert "0000180f-0000-1000-8000-00805f9b34fb" in uuids and NUS_SERVICE in uuids, uuids
    nus = next(s for s in svcs.services if s.uuid == NUS_SERVICE)
    assert nus.characteristics[0].handle == ECHO and nus.characteristics[0].descriptors[0].handle == CCCD

    assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, BATTERY)) == bytes([87])
    await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"ping", True)
    await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"pong", False)
    await asyncio.sleep(0.2)
    assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, ECHO)) == b"pong"
    # Writes without response are not acknowledged; the proxy must apply them, and the read queued after
    # them, in order.
    for i in range(50):
        await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"w%02d" % i, False)
    assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, ECHO)) == b"w49"
    print("read/write ok")

    notes = []
    stop, remove = await cli.bluetooth_gatt_start_notify(FAKE_ADDR, ECHO, lambda h, d: notes.append(bytes(d)))
    await cli.bluetooth_gatt_write_descriptor(FAKE_ADDR, CCCD, b"\x01\x00")
    await asyncio.sleep(2.5)
    await stop()
    remove()
    print("notifications:", notes)
    assert len(notes) >= 2

    try:
        await cli.bluetooth_gatt_read(FAKE_ADDR, 0x0099)
        raise AssertionError("read of a bad handle succeeded")
    except Exception as e:  # BluetoothGATTAPIError
        print("bad handle ->", type(e).__name__)

    paired = await cli.bluetooth_device_pair(FAKE_ADDR)
    print("pair:", paired)
    assert paired.paired and paired.error == 0, paired

    await cli.bluetooth_device_disconnect(FAKE_ADDR)
    await asyncio.sleep(0.3)
    print("states:", states, "free:", free[-1])
    assert states[-1][0] is False and free[-1][0] == 3

    paired = await cli.bluetooth_device_pair(FAKE_ADDR)
    print("pair while disconnected:", paired)
    assert not paired.paired and paired.error == -1, paired

    # Unpairing drops the connection (BlueZ removes the device); the reply must come first, or aioesphomeapi
    # raises BluetoothConnectionDroppedError.
    await connect()
    unpaired = await cli.bluetooth_device_unpair(FAKE_ADDR)
    print("unpair:", unpaired)
    assert unpaired.success and unpaired.error == 0, unpaired
    await asyncio.sleep(0.3)
    print("states:", states[-1], "free:", free[-1])
    assert states[-1][0] is False and free[-1][0] == 3

    unpaired = await cli.bluetooth_device_unpair(0xAABBCCDDEE99)
    print("unpair unknown device:", unpaired)
    assert not unpaired.success and unpaired.error == 0x85, unpaired

    unsub()
    unsub_free()
    unsub_scanner()
    await cli.disconnect()
    print("E2E OK")


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1",
                     int(sys.argv[2]) if len(sys.argv) > 2 else 6053))
