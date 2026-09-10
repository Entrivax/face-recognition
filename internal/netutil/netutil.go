// Package netutil holds the serve command's network helpers: normalizing a
// user-supplied listen address, binding the listener, and deriving the URLs a
// bound listener is actually reachable on (a specific address yields one URL;
// a wildcard listener lists every matching interface).
package netutil

import (
	"net"
	"strconv"
	"strings"
)

// Listen normalizes a user-supplied serve address and binds it with
// net.Listen("tcp", ...). A bare port number ("8080") is treated as ":8080";
// every other form is passed to net.Listen unchanged.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", normalizeListenAddr(addr))
}

// normalizeListenAddr maps a bare port number to a wildcard host-port; every
// other form is passed through unchanged (net.Listen reports invalid values).
func normalizeListenAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	if _, err := strconv.Atoi(addr); err == nil {
		return ":" + addr
	}
	return addr
}

// URLs returns the URLs a bound listener address is reachable on. A specific
// listen host yields a single URL; a wildcard (empty host, 0.0.0.0 or ::)
// yields localhost plus one URL per matching interface address.
func URLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{urlFor(listen, "")}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{urlFor(host, port)}
	}
	// Wildcard host: the socket covers every interface. 0.0.0.0 is IPv4-only;
	// an empty host or :: is dual-stack (accepts IPv4-mapped by default).
	urls := []string{urlFor("localhost", port)}
	return append(urls, interfaceURLs(port, true, host != "0.0.0.0")...)
}

// interfaceURLs lists one URL per distinct non-link-local address on an up,
// non-loopback interface, restricted to the address families the listener
// accepts. Best-effort: unusable interfaces are skipped.
func interfaceURLs(port string, want4, want6 bool) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var urls []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			v4 := ip.To4() != nil
			if (v4 && !want4) || (!v4 && !want6) || ip.IsLinkLocalUnicast() {
				continue // not routable from another host / needs an ifzone
			}
			host := ip.String()
			if seen[host] {
				continue
			}
			seen[host] = true
			urls = append(urls, urlFor(host, port))
		}
	}
	return urls
}

// urlFor renders host:port as an http URL, bracketing IPv6 literals.
func urlFor(host, port string) string {
	if strings.Contains(host, ":") {
		return "http://[" + host + "]:" + port
	}
	return "http://" + host + ":" + port
}
