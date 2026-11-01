package netif

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Interface types of tunnels and PPP links (ipifcons.h).
const (
	ifTypePPP    = 23
	ifTypeTunnel = 131
)

// Virtual returns the interfaces that look virtual, by index, with the reason. On Windows, the default mDNS
// interface list leaves them out: Hyper-V's Default Switch and WSL's vEthernet are up with IPv4 on every
// machine that has them, and VPN adapters come and go.
func Virtual() map[int]string {
	out := map[int]string{}
	for _, a := range adapters() {
		ad := Adapter{
			Name:        windows.UTF16PtrToString(a.FriendlyName),
			Description: windows.UTF16PtrToString(a.Description),
			MAC:         net.HardwareAddr(a.PhysicalAddress[:min(a.PhysicalAddressLength, uint32(len(a.PhysicalAddress)))]),
			Tunnel:      a.IfType == ifTypeTunnel || a.IfType == ifTypePPP,
		}
		for g := a.FirstGatewayAddress; g != nil; g = g.Next {
			if ip := g.Address.IP(); ip != nil && ip.To4() != nil {
				ad.Gateway = true
			}
		}
		if r := VirtualReason(ad); r != "" {
			idx := int(a.IfIndex)
			if idx == 0 {
				idx = int(a.Ipv6IfIndex)
			}
			out[idx] = r
		}
	}
	return out
}

// adapters lists the adapters with their gateways. The list points into a buffer that stays reachable through
// the returned values.
func adapters() []*windows.IpAdapterAddresses {
	size := uint32(15000)
	for range 3 {
		buf := make([]byte, size)
		p := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_GATEWAYS|
			windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER, 0, p, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil || size == 0 {
			return nil
		}
		var out []*windows.IpAdapterAddresses
		for ; p != nil; p = p.Next {
			out = append(out, p)
		}
		return out
	}
	return nil
}
