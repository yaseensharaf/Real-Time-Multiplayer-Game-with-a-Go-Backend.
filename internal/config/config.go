// Package config loads server settings from the environment.
//
// Everything deployment-specific is read here rather than hardcoded, so the
// same binary runs locally and in a container without a rebuild.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr            string
	AllowedOrigins  []string
	GracePeriod     time.Duration
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	PingInterval    time.Duration
	MaxMessageBytes int64
	SendBuffer      int
	LogLevel        string
	LogFormat       string
	WebDir          string
}

// Default returns settings suitable for local development.
func Default() Config {
	return Config{
		Addr:            ":8080",
		AllowedOrigins:  nil, // nil means same-origin only; see transport.
		GracePeriod:     30 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		ReadTimeout:     60 * time.Second,
		WriteTimeout:    10 * time.Second,
		PingInterval:    25 * time.Second,
		MaxMessageBytes: 1024,
		SendBuffer:      16,
		LogLevel:        "info",
		LogFormat:       "text",
		WebDir:          "./web",
	}
}

// Load reads configuration from the environment, falling back to Default for
// anything unset, and validates the result.
func Load() (Config, error) {
	c := Default()

	c.Addr = envStr("ADDR", c.Addr)
	c.LogLevel = envStr("LOG_LEVEL", c.LogLevel)
	c.LogFormat = envStr("LOG_FORMAT", c.LogFormat)
	c.WebDir = envStr("WEB_DIR", c.WebDir)

	if v := os.Getenv("ALLOWED_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				c.AllowedOrigins = append(c.AllowedOrigins, o)
			}
		}
	}

	var err error
	if c.GracePeriod, err = envDur("GRACE_PERIOD", c.GracePeriod); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envDur("SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return c, err
	}
	if c.ReadTimeout, err = envDur("READ_TIMEOUT", c.ReadTimeout); err != nil {
		return c, err
	}
	if c.WriteTimeout, err = envDur("WRITE_TIMEOUT", c.WriteTimeout); err != nil {
		return c, err
	}
	if c.PingInterval, err = envDur("PING_INTERVAL", c.PingInterval); err != nil {
		return c, err
	}
	if c.SendBuffer, err = envInt("SEND_BUFFER", c.SendBuffer); err != nil {
		return c, err
	}

	return c, c.Validate()
}

// Validate rejects settings that would misbehave at runtime. Failing at
// startup with a clear message beats a server that silently drops every
// connection because its ping interval exceeds its read timeout.
func (c Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("ADDR must not be empty")
	}
	if c.PingInterval >= c.ReadTimeout {
		return fmt.Errorf("PING_INTERVAL (%s) must be shorter than READ_TIMEOUT (%s), "+
			"otherwise healthy connections time out between pings", c.PingInterval, c.ReadTimeout)
	}
	if c.GracePeriod < 0 {
		return fmt.Errorf("GRACE_PERIOD must not be negative")
	}
	if c.SendBuffer < 1 {
		return fmt.Errorf("SEND_BUFFER must be at least 1")
	}
	if c.MaxMessageBytes < 1 {
		return fmt.Errorf("MaxMessageBytes must be at least 1")
	}
	return nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
