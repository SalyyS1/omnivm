package config

import (
	"testing"
	"time"
)

func validBaseConfig() *Config {
	cfg := DefaultConfig()
	cfg.DNSServer = "1.1.1.1"
	return cfg
}

func TestConfigValidate_DefaultConfig(t *testing.T) {
	cfg := validBaseConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() unexpected error: %v", err)
	}
}

func TestConfigValidate_NilReceiver(t *testing.T) {
	var cfg *Config
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for nil *Config")
	}
}

func TestConfigValidate_InvalidCases(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "empty_data_dir",
			mutate: func(c *Config) { c.DataDir = "" },
		},
		{
			name:   "empty_socket_path",
			mutate: func(c *Config) { c.SocketPath = "" },
		},
		{
			name:   "zero_memory",
			mutate: func(c *Config) { c.DefaultMemoryMB = 0 },
		},
		{
			name:   "excessive_memory",
			mutate: func(c *Config) { c.DefaultMemoryMB = 2 * 1024 * 1024 },
		},
		{
			name:   "zero_vcpus",
			mutate: func(c *Config) { c.DefaultVCPUs = 0 },
		},
		{
			name:   "negative_vcpus",
			mutate: func(c *Config) { c.DefaultVCPUs = -2 },
		},
		{
			name:   "zero_min_disk",
			mutate: func(c *Config) { c.MinDiskMB = 0 },
		},
		{
			name: "max_disk_below_min_disk",
			mutate: func(c *Config) {
				c.MinDiskMB = 2048
				c.MaxDiskMB = 1024
			},
		},
		{
			name:   "negative_pause_after_idle",
			mutate: func(c *Config) { c.PauseAfterIdle = -1 * time.Second },
		},
		{
			name:   "negative_stop_after_idle",
			mutate: func(c *Config) { c.StopAfterIdle = -5 * time.Second },
		},
		{
			name:   "empty_router_addr",
			mutate: func(c *Config) { c.RouterAddr = "" },
		},
		{
			name:   "malformed_router_addr",
			mutate: func(c *Config) { c.RouterAddr = "127.0.0.1" },
		},
		{
			name:   "out_of_range_router_port",
			mutate: func(c *Config) { c.RouterAddr = "127.0.0.1:70000" },
		},
		{
			name:   "unsupported_network_backend",
			mutate: func(c *Config) { c.NetworkBackend = "slirp" },
		},
		{
			name:   "invalid_dns_server",
			mutate: func(c *Config) { c.DNSServer = "not-an-ip" },
		},
		{
			name:   "ipv6_dns_server_rejected",
			mutate: func(c *Config) { c.DNSServer = "2001:4860:4860::8888" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBaseConfig()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("%s: expected validation error, got nil", tc.name)
			}
		})
	}
}

func TestConfigValidate_EdgeValidCases(t *testing.T) {
	cfg := validBaseConfig()
	cfg.RouterAddr = "127.0.0.1:0" // ephemeral port allowed for tests
	cfg.PauseAfterIdle = 0
	cfg.StopAfterIdle = 0
	cfg.MinDiskMB = 512
	cfg.MaxDiskMB = 512
	cfg.NetworkBackend = "tap"
	cfg.DNSServer = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid edge config, got error: %v", err)
	}
}
