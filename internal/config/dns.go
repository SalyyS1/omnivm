package config

import (
	"bufio"
	"io"
	"log"
	"net"
	"os"
	"strings"
)

// fallbackDNSServer is used only when neither OVM_DNS nor the host resolver
// configuration yields a usable address. It is logged when used so operators
// notice and set OVM_DNS explicitly.
const fallbackDNSServer = "8.8.8.8"

// hostResolvConfPaths are checked in order. systemd-resolved keeps the real
// upstream servers in /run/systemd/resolve/resolv.conf, while /etc/resolv.conf
// usually points at its 127.0.0.53 stub, which a guest cannot reach.
var hostResolvConfPaths = []string{
	"/run/systemd/resolve/resolv.conf",
	"/etc/resolv.conf",
}

// ResolveDNS fills DNSServer when it was not set explicitly. Guests reach the
// network through NAT, so only a non-loopback IPv4 resolver of the host is
// usable.
func (c *Config) ResolveDNS() {
	if c.DNSServer != "" {
		if ip := net.ParseIP(c.DNSServer); ip == nil || ip.To4() == nil {
			log.Printf("OVM_DNS=%q is not an IPv4 address, ignoring it", c.DNSServer)
			c.DNSServer = ""
		} else {
			return
		}
	}
	for _, path := range hostResolvConfPaths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		server := firstUsableNameserver(f)
		f.Close()
		if server != "" {
			c.DNSServer = server
			return
		}
	}
	log.Printf("no usable host DNS resolver found; guests will use %s (set OVM_DNS to override)", fallbackDNSServer)
	c.DNSServer = fallbackDNSServer
}

// firstUsableNameserver returns the first non-loopback IPv4 `nameserver`
// entry of a resolv.conf stream, or "" if there is none.
func firstUsableNameserver(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(fields[1])
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return ""
}
