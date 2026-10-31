"""Connects through a running btproxy to one device and lists its services: python test/connect.py host MAC"""

import asyncio
import sys

from aioesphomeapi import APIClient


async def main(host: str, mac: str) -> None:
    addr = int(mac.replace(":", ""), 16)
    cli = APIClient(host, 6053, None)
    await cli.connect(login=True)
    info = await cli.device_info()
    done = asyncio.get_running_loop().create_future()

    def on_state(conn: bool, mtu: int, err: int) -> None:
        print("state:", conn, mtu, err)
        if not done.done():
            done.set_result(conn)

    await cli.bluetooth_device_connect(addr, on_state, feature_flags=info.bluetooth_proxy_feature_flags,
                                       address_type=0, timeout=30)
    if await asyncio.wait_for(done, 35):
        svcs = await cli.bluetooth_gatt_get_services(addr)
        for s in svcs.services:
            print(s.uuid, [(hex(c.handle), c.uuid[4:8], c.properties) for c in s.characteristics])
        await cli.bluetooth_device_disconnect(addr)
    await cli.disconnect()


asyncio.run(main(sys.argv[1], sys.argv[2]))
