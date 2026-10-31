"""Connects to a running btproxy, prints device info and a few seconds of advertisements."""

import asyncio
import sys

from aioesphomeapi import APIClient


async def main(host: str, seconds: float) -> None:
    cli = APIClient(host, 6053, None)
    await cli.connect(login=True)
    info = await cli.device_info()
    print("device:", info.name, info.mac_address, "bt", info.bluetooth_mac_address,
          "flags", info.bluetooth_proxy_feature_flags)
    seen = {}
    unsub = cli.subscribe_bluetooth_le_raw_advertisements(
        lambda r: seen.update({a.address: (a.rssi, bytes(a.data).hex()) for a in r.advertisements}))
    await asyncio.sleep(seconds)
    unsub()
    for addr, (rssi, data) in sorted(seen.items(), key=lambda x: -x[1][0]):
        print(f"{addr:012X} {rssi:4d} {data}")
    print(len(seen), "devices")
    await cli.disconnect()


asyncio.run(main(sys.argv[1], float(sys.argv[2]) if len(sys.argv) > 2 else 10))
