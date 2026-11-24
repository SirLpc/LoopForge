package gitegress

import "net/netip"

// PrivateHTTPAllowed applies the existing egress policy to a private IP literal.
func PrivateHTTPAllowed(ip netip.Addr) bool {
	return ip.IsPrivate() && checkAddress(netip.AddrPortFrom(ip, 80).String(), allowedPrefixes()) == nil
}
