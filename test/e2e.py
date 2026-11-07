"""End-to-end check of btproxy with aioesphomeapi, the client library Home Assistant uses.

Run btproxy with -backend fake, then: python test/e2e.py [host] [port] [encryption key]

With a key (btproxy started with the same -encryption-key), the session is encrypted, and a plaintext client
and a client with the wrong key must be refused with the errors Home Assistant turns into a reauth prompt.

Written against aioesphomeapi 46.28.0, which ci.yml pins. Each step waits for what it needs, with a deadline,
rather than for a fixed time, and the whole run has a deadline too, so a proxy that never answers fails rather
than hangs.
"""

import asyncio
import base64
import sys
import time
from collections.abc import Callable

from aioesphomeapi import (APIClient, BinarySensorInfo, BluetoothScannerMode, BluetoothScannerState, ButtonInfo,
                           EntityCategory, InvalidEncryptionKeyAPIError, LogLevel, RequiresEncryptionAPIError,
                           SensorInfo, SwitchInfo, TextSensorInfo)
from aioesphomeapi.core import APIConnectionError, BluetoothConnectionParamsAPIError, BluetoothGATTAPIError

FAKE_ADDR = 0xAABBCCDDEE01
BATTERY, ECHO, CCCD = 0x0003, 0x0013, 0x0014
NUS_SERVICE = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
ADVERT_INTERVAL = 0.5  # the fake radio advertises this often
ADVERT_FLUSH = 0.1  # the proxy sends a batch of advertisements this long after its first one
TOTAL_TIMEOUT = 120

RUNNING, IDLE = BluetoothScannerState.RUNNING, BluetoothScannerState.IDLE
ACTIVE, PASSIVE = BluetoothScannerMode.ACTIVE, BluetoothScannerMode.PASSIVE


async def wait_for(cond: Callable[[], object], what: str, timeout: float = 5) -> None:
    """Polls until cond holds, and fails after timeout seconds. Callbacks run on this loop, so polling sees
    their effect as soon as the proxy's message has arrived."""
    deadline = time.monotonic() + timeout
    while not cond():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        await asyncio.sleep(0.02)


async def expect_refused(cli: APIClient, exc: type) -> None:
    try:
        await cli.connect(login=True)
    except exc as e:
        print("refused:", type(e).__name__)
        return
    finally:
        await cli.disconnect(force=True)
    raise AssertionError(f"connection not refused with {exc.__name__}")


class Session:
    """One client session, with what its subscriptions have reported so far."""

    def __init__(self, host: str, port: int, key: str | None) -> None:
        self.host, self.port, self.key = host, port, key
        self.cli = APIClient(host, port, None, noise_psk=key or None)
        self.stopped = asyncio.Event()  # the proxy closed the session
        self.states = {}  # entity key -> latest state
        self.logs = []  # (level, message)
        self.scanner = []  # (state, mode, configured mode)
        self.adverts = []
        self.free = []  # (free, limit, allocated)
        self.conn = []  # (connected, mtu, error) of the fake device's connection
        self.unsubs = []

    async def connect(self) -> None:
        async def on_stop(expected: bool) -> None:
            self.stopped.set()

        await self.cli.connect(on_stop=on_stop, login=True)
        self.info = info = await self.cli.device_info()
        caps = await self.cli.device_capabilities()
        print("capabilities:", caps)
        print("device:", info.name, info.mac_address, "bt", info.bluetooth_mac_address,
              "flags", info.bluetooth_proxy_feature_flags)
        assert info.bluetooth_proxy_feature_flags & 0b10, "active connections not advertised"
        assert info.bluetooth_proxy_feature_flags & 0b1000, "pairing not advertised"
        assert info.bluetooth_proxy_feature_flags & 0x80, "connection parameters not advertised"

    async def list_entities(self) -> None:
        entities, services = await self.cli.list_entities_services()
        assert services == []
        self.sensors = sensors = {e.object_id: e for e in entities if isinstance(e, SensorInfo)}
        print("sensors:", sorted(sensors))
        for oid in ("uptime", "connection_attempts", "connections_made", "advertisement_rate",
                    "free_connection_slots"):
            assert oid in sensors, f"sensor {oid} missing"
        assert all(e.entity_category == EntityCategory.DIAGNOSTIC for e in sensors.values())
        self.by_id = by_id = {e.object_id: e for e in entities}
        print("entities:", sorted(by_id))
        self.stack, self.scanning = by_id["bluetooth_stack"], by_id["scanning"]
        assert isinstance(self.stack, BinarySensorInfo) and self.stack.entity_category == EntityCategory.DIAGNOSTIC
        assert isinstance(self.scanning, SwitchInfo) and self.scanning.entity_category == EntityCategory.CONFIG
        for oid in ("adapter_address", "advertisement_source", "last_problem"):
            assert isinstance(by_id[oid], TextSensorInfo), oid
        for oid in ("restart_proxy", "restart_bluetooth"):
            assert isinstance(by_id[oid], ButtonInfo) and by_id[oid].disabled_by_default, oid

    async def check_states(self) -> None:
        states, sensors, by_id = self.states, self.sensors, self.by_id
        self.cli.subscribe_states(lambda s: states.__setitem__(s.key, s))
        wanted = [sensors["uptime"], sensors["free_connection_slots"], self.stack, self.scanning,
                  by_id["adapter_address"], by_id["advertisement_source"]]
        await wait_for(lambda: all(e.key in states for e in wanted), "the initial states")
        uptime = states[sensors["uptime"].key]
        assert not uptime.missing_state and uptime.state >= 0, f"uptime state {uptime}"
        assert states[sensors["free_connection_slots"].key].state == 3
        assert states[self.stack.key].state is True, states[self.stack.key]
        assert states[self.scanning.key].state is True, states[self.scanning.key]
        adapter = states[by_id["adapter_address"].key]
        assert adapter.state == self.info.bluetooth_mac_address, adapter
        assert states[by_id["advertisement_source"].key].state == "Fake"

    async def check_logs(self) -> None:
        logs = self.logs
        self.unsubs.append(self.cli.subscribe_logs(lambda m: logs.append((m.level, m.message.decode())),
                                                   LogLevel.LOG_LEVEL_DEBUG, dump_config=True))
        await wait_for(lambda: logs, "the config dump")
        assert logs[0][0] == LogLevel.LOG_LEVEL_CONFIG and "Bluetooth Proxy:" in logs[0][1], logs
        print("config dump:", repr(logs[0][1]))

    async def check_scanner(self) -> None:
        scanner, adverts, free = self.scanner, self.adverts, self.free
        # Registered first: the state report answers the advertisement subscription.
        self.unsubs.append(self.cli.subscribe_bluetooth_scanner_state(
            lambda s: scanner.append((s.state, s.mode, s.configured_mode))))
        self.unsubs.append(self.cli.subscribe_bluetooth_le_raw_advertisements(
            lambda r: adverts.extend(r.advertisements)))
        self.unsubs.append(self.cli.subscribe_bluetooth_connections_free(
            lambda f, l, a: free.append((f, l, list(a)))))
        await wait_for(lambda: any(a.address == FAKE_ADDR and a.rssi == -60 for a in adverts),
                       "an advertisement from the fake device")
        print("adverts:", len(adverts), "first data", bytes(adverts[0].data).hex())
        await wait_for(lambda: free, "the free connections report")
        assert free[0][:2] == (3, 3), free

        # Started with the default -scan-mode active; Home Assistant may switch modes at run time, while the
        # configured mode stays.
        await wait_for(lambda: scanner, "the scanner state")
        assert scanner[-1] == (RUNNING, ACTIVE, ACTIVE), scanner
        self.cli.bluetooth_scanner_set_mode(PASSIVE)
        await wait_for(lambda: scanner[-1] == (RUNNING, PASSIVE, ACTIVE), "passive mode")
        self.cli.bluetooth_scanner_set_mode(ACTIVE)
        await wait_for(lambda: scanner[-1] == (RUNNING, ACTIVE, ACTIVE), "active mode again")
        print("scanner states:", [(s.name, m.name, c.name) for s, m, c in scanner])

    async def check_scanning_switch(self) -> None:
        """The scanning switch stops the advertisements, and the stack still counts as working."""
        scanner, adverts, states, key = self.scanner, self.adverts, self.states, self.scanning.key
        self.cli.switch_command(key, False)
        await wait_for(lambda: scanner[-1][0] == IDLE and states[key].state is False, "scanning off")
        assert scanner[-1] == (IDLE, ACTIVE, ACTIVE), scanner
        assert states[self.stack.key].state is True, states[self.stack.key]
        # Nothing may arrive now, which only a quiet spell shows: a batch collected before the switch may still be
        # on its way, then a few advertising intervals pass without one.
        await asyncio.sleep(2 * ADVERT_FLUSH)
        adverts.clear()
        await asyncio.sleep(3 * ADVERT_INTERVAL)
        assert not adverts, adverts
        self.cli.switch_command(key, True)
        await wait_for(lambda: scanner[-1] == (RUNNING, ACTIVE, ACTIVE) and states[key].state is True,
                       "scanning on again")
        await wait_for(lambda: adverts, "advertisements after switching scanning back on")
        print("scanning switch ok")

    async def press_restart_bluetooth(self) -> None:
        """The fake backend has nothing to restart; the press must not disturb the session, so the request after
        it, on the same connection, is answered."""
        self.cli.button_command(self.by_id["restart_bluetooth"].key)
        assert (await self.cli.device_info()).name == self.info.name
        print("restart_bluetooth pressed")

    async def connect_device(self) -> None:
        connected = asyncio.get_running_loop().create_future()

        def on_state(conn: bool, mtu: int, err: int) -> None:
            self.conn.append((conn, mtu, err))
            if not connected.done():
                connected.set_result((conn, mtu, err))

        await self.cli.bluetooth_device_connect(
            FAKE_ADDR, on_state, feature_flags=self.info.bluetooth_proxy_feature_flags, address_type=1)
        print("connect:", await asyncio.wait_for(connected, 5))

    async def wait_disconnected(self) -> None:
        """Waits for the device's connection to report closed and its slot to be free again."""
        conn, free = self.conn, self.free
        await wait_for(lambda: conn[-1][0] is False and free[-1][0] == 3, "the disconnect and the freed slot")
        print("states:", conn[-1], "free:", free[-1])

    async def check_gatt(self) -> None:
        cli = self.cli
        svcs = await cli.bluetooth_gatt_get_services(FAKE_ADDR)
        uuids = [s.uuid for s in svcs.services]
        print("services:", uuids)
        assert "0000180f-0000-1000-8000-00805f9b34fb" in uuids and NUS_SERVICE in uuids, uuids
        nus = next(s for s in svcs.services if s.uuid == NUS_SERVICE)
        assert nus.characteristics[0].handle == ECHO and nus.characteristics[0].descriptors[0].handle == CCCD

        assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, BATTERY)) == bytes([87])
        await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"ping", True)
        await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"pong", False)
        # Writes without response are not acknowledged; the proxy must apply them, and the read queued after
        # them, in order.
        assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, ECHO)) == b"pong"
        for i in range(50):
            await cli.bluetooth_gatt_write(FAKE_ADDR, ECHO, b"w%02d" % i, False)
        assert bytes(await cli.bluetooth_gatt_read(FAKE_ADDR, ECHO)) == b"w49"
        print("read/write ok")

    async def check_notify(self) -> None:
        notes = []
        stop, remove = await self.cli.bluetooth_gatt_start_notify(FAKE_ADDR, ECHO, lambda h, d: notes.append(bytes(d)))
        await self.cli.bluetooth_gatt_write_descriptor(FAKE_ADDR, CCCD, b"\x01\x00")
        await wait_for(lambda: len(notes) >= 2, "two notifications")  # the fake notifies once a second
        await stop()
        remove()
        print("notifications:", notes)

    async def check_bad_handle(self) -> None:
        try:
            await self.cli.bluetooth_gatt_read(FAKE_ADDR, 0x0099)
        except BluetoothGATTAPIError as e:
            print("bad handle ->", type(e).__name__)
            return
        raise AssertionError("read of a bad handle succeeded")

    async def check_pairing(self) -> None:
        paired = await self.cli.bluetooth_device_pair(FAKE_ADDR)
        print("pair:", paired)
        assert paired.paired and paired.error == 0, paired
        # 30-50 ms interval, 5 s supervision timeout; aioesphomeapi raises on a nonzero error.
        await self.cli.bluetooth_device_set_connection_params(FAKE_ADDR, 24, 40, 0, 500)
        print("connection params ok")

    async def disconnect_device(self) -> None:
        await self.cli.bluetooth_device_disconnect(FAKE_ADDR)
        await self.wait_disconnected()

    async def check_disconnected_errors(self) -> None:
        paired = await self.cli.bluetooth_device_pair(FAKE_ADDR)
        print("pair while disconnected:", paired)
        assert not paired.paired and paired.error == -1, paired
        try:
            await self.cli.bluetooth_device_set_connection_params(FAKE_ADDR, 24, 40, 0, 500)
        except BluetoothConnectionParamsAPIError as e:
            print("connection params while disconnected ->", e.error_code)
            assert e.error_code == -1, e.error_code
            return
        raise AssertionError("connection params while disconnected succeeded")

    async def check_unpair(self) -> None:
        """Unpairing drops the connection (BlueZ removes the device); the reply must come first, or aioesphomeapi
        raises BluetoothConnectionDroppedError."""
        await self.connect_device()
        unpaired = await self.cli.bluetooth_device_unpair(FAKE_ADDR)
        print("unpair:", unpaired)
        assert unpaired.success and unpaired.error == 0, unpaired
        await self.wait_disconnected()
        # Nothing to remove counts as success, as with BlueZ.
        unpaired = await self.cli.bluetooth_device_unpair(0xAABBCCDDEE99)
        print("unpair unknown device:", unpaired)
        assert unpaired.success and unpaired.error == 0, unpaired

    async def check_log_lines(self) -> None:
        logs = self.logs

        def connected():
            return [m for lv, m in logs if lv == LogLevel.LOG_LEVEL_INFO and "]: connected " in m]

        await wait_for(connected, "the connection's log line")
        print("log lines:", len(logs), "e.g.", repr(connected()[:1]))
        assert connected()[0].startswith("\x1b[0;32m[I][server:"), logs  # green, as ESPHome colours info

    async def restart_proxy(self) -> None:
        """Last, as it ends this session: the proxy drops it and starts again on the same port."""
        for unsub in self.unsubs:
            unsub()
        self.cli.button_command(self.by_id["restart_proxy"].key)
        await asyncio.wait_for(self.stopped.wait(), 5)
        await self.cli.disconnect(force=True)
        again = APIClient(self.host, self.port, None, noise_psk=self.key or None)
        deadline = time.monotonic() + 10
        while True:
            try:
                await again.connect(login=True)
                break
            except APIConnectionError as e:
                if time.monotonic() > deadline:
                    raise AssertionError(f"the proxy did not come back: {e}") from e
                await asyncio.sleep(0.1)
        assert (await again.device_info()).name == self.info.name
        restarted = {}
        again.subscribe_states(lambda s: restarted.__setitem__(s.key, s))
        uptime = self.sensors["uptime"].key
        await wait_for(lambda: uptime in restarted, "the uptime after the restart")
        assert restarted[uptime].state < 5, restarted[uptime]
        await again.disconnect()
        print("restart_proxy ok")


async def main(host: str, port: int, key: str | None) -> None:
    if key:
        await expect_refused(APIClient(host, port, None), RequiresEncryptionAPIError)
        wrong = base64.b64encode(bytes(32)).decode()
        await expect_refused(APIClient(host, port, None, noise_psk=wrong), InvalidEncryptionKeyAPIError)
    s = Session(host, port, key)
    await s.connect()
    await s.list_entities()
    await s.check_states()
    await s.check_logs()
    await s.check_scanner()
    await s.check_scanning_switch()
    await s.press_restart_bluetooth()
    await s.connect_device()
    await s.check_gatt()
    await s.check_notify()
    await s.check_bad_handle()
    await s.check_pairing()
    await s.disconnect_device()
    await s.check_disconnected_errors()
    await s.check_unpair()
    await s.check_log_lines()
    await s.restart_proxy()
    print("E2E OK")


if __name__ == "__main__":
    asyncio.run(asyncio.wait_for(main(sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1",
                                      int(sys.argv[2]) if len(sys.argv) > 2 else 6053,
                                      sys.argv[3] if len(sys.argv) > 3 else None), TOTAL_TIMEOUT))
