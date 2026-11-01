//go:build !windows

package netif

import "net"

// Virtual returns the interfaces whose names mark them as container, VM or VPN plumbing, by index, with the
// reason. It goes by name only: on Linux, bridges such as OpenWrt's br-lan, VLAN devices and macvlans often have
// locally administered MACs and are the very interfaces to advertise on, and a Linux VM's own NIC has a
// hypervisor's OUI, so neither MAC test is safe there. A proxy in a container runs with the host's network (the
// kernel allows Bluetooth sockets only in the initial network namespace), so it sees these interfaces too.
func Virtual() map[int]string {
	ifs, _ := net.Interfaces()
	out := map[int]string{}
	for _, i := range ifs {
		if r := VirtualName(i.Name); r != "" {
			out[i.Index] = r
		}
	}
	return out
}
