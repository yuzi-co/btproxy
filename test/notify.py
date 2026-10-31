"""Connects through btproxy and enables notifications on one handle: python test/notify.py host MAC handle"""

import asyncio
import sys
import time

from aioesphomeapi import APIClient


async def main(host: str, mac: str, handle: int) -> None:
    addr = int(mac.replace(":", ""), 16)
    cli = APIClient(host, 6053, None)
    await cli.connect(login=True)
    info = await cli.device_info()
    done = asyncio.get_running_loop().create_future()
    cb = lambda c, m, e: done.done() or done.set_result(c)
    await cli.bluetooth_device_connect(addr, cb, feature_flags=info.bluetooth_proxy_feature_flags,
                                       address_type=0, timeout=30)
    print("connected:", await asyncio.wait_for(done, 35))
    t = time.monotonic()
    try:
        stop, remove = await cli.bluetooth_gatt_start_notify(addr, handle, lambda h, d: print("notify", bytes(d).hex()),
                                                              timeout=40)
        print(f"start_notify ok in {time.monotonic() - t:.1f}s")
    except Exception as e:
        print(f"start_notify failed after {time.monotonic() - t:.1f}s: {e!r}")
    await cli.bluetooth_device_disconnect(addr)
    await cli.disconnect()


asyncio.run(main(sys.argv[1], sys.argv[2], int(sys.argv[3], 0)))
