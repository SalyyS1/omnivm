package config

import (
	"strings"
	"testing"
)

func TestFirstUsableNameserver(t *testing.T) {
	cases := []struct {
		name string
		conf string
		want string
	}{
		{"systemd stub only", "nameserver 127.0.0.53\noptions edns0\n", ""},
		{"skips loopback and ipv6", "nameserver 127.0.0.1\nnameserver ::1\nnameserver 2001:db8::1\nnameserver 10.0.0.2\n", "10.0.0.2"},
		{"first usable wins", "# comment\nsearch example.com\nnameserver 1.1.1.1\nnameserver 9.9.9.9\n", "1.1.1.1"},
		{"ignores garbage", "nameserver not-an-ip\nnameserver\nnameserver 0.0.0.0\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstUsableNameserver(strings.NewReader(tc.conf)); got != tc.want {
				t.Fatalf("firstUsableNameserver() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveDNSKeepsValidExplicitServer(t *testing.T) {
	c := &Config{DNSServer: "192.0.2.53"}
	c.ResolveDNS()
	if c.DNSServer != "192.0.2.53" {
		t.Fatalf("explicit DNS server overwritten: %q", c.DNSServer)
	}
}

func TestResolveDNSRejectsInvalidExplicitServer(t *testing.T) {
	c := &Config{DNSServer: "dns.example.com"}
	c.ResolveDNS()
	if c.DNSServer == "dns.example.com" || c.DNSServer == "" {
		t.Fatalf("invalid explicit DNS server not replaced: %q", c.DNSServer)
	}
}
