package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

// TestPingMustBeShorterThanReadTimeout guards a misconfiguration that is
// invisible until production: if pings arrive less often than the read
// deadline, healthy players are disconnected on a timer.
func TestPingMustBeShorterThanReadTimeout(t *testing.T) {
	c := Default()
	c.PingInterval = c.ReadTimeout
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "PING_INTERVAL") {
		t.Fatalf("error should name the offending setting, got: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty addr", func(c *Config) { c.Addr = "" }},
		{"negative grace", func(c *Config) { c.GracePeriod = -time.Second }},
		{"zero send buffer", func(c *Config) { c.SendBuffer = 0 }},
		{"zero max message", func(c *Config) { c.MaxMessageBytes = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("ADDR", ":9999")
	t.Setenv("GRACE_PERIOD", "45s")
	t.Setenv("ALLOWED_ORIGINS", "https://a.example, https://b.example")
	t.Setenv("SEND_BUFFER", "64")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Addr != ":9999" {
		t.Errorf("Addr = %q", c.Addr)
	}
	if c.GracePeriod != 45*time.Second {
		t.Errorf("GracePeriod = %v", c.GracePeriod)
	}
	if len(c.AllowedOrigins) != 2 || c.AllowedOrigins[1] != "https://b.example" {
		t.Errorf("AllowedOrigins = %v (whitespace should be trimmed)", c.AllowedOrigins)
	}
	if c.SendBuffer != 64 {
		t.Errorf("SendBuffer = %d", c.SendBuffer)
	}
}

func TestLoadRejectsMalformedDuration(t *testing.T) {
	t.Setenv("GRACE_PERIOD", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed duration")
	}
}
