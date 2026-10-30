// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"strings"
	"time"
)

type Config struct {
	ListenAddr, DataDir, APIToken, LogLevel string
	RefreshInterval, RequestDelay           time.Duration
	Horizon, Retention                      time.Duration
}

// Load reads configuration using getenv (typically os.Getenv).
func Load(getenv func(string) string) (Config, error) {
	str := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ListenAddr: str("LISTEN_ADDR", ":8080"),
		DataDir:    str("DATA_DIR", "/data"),
		APIToken:   getenv("API_TOKEN"),
		LogLevel:   strings.ToLower(str("LOG_LEVEL", "info")),
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL: invalid value %q (want debug|info|warn|error)", getenv("LOG_LEVEL"))
	}
	dur := func(k, def string, allowZero bool) (time.Duration, error) {
		d, err := time.ParseDuration(str(k, def))
		if err != nil {
			return 0, fmt.Errorf("%s: %w", k, err)
		}
		if d < 0 || (d == 0 && !allowZero) {
			return 0, fmt.Errorf("%s: must be %s, got %s", k, map[bool]string{true: "non-negative", false: "positive"}[allowZero], d)
		}
		return d, nil
	}
	var err error
	if c.RefreshInterval, err = dur("REFRESH_INTERVAL", "3h", false); err != nil {
		return Config{}, err
	}
	if c.RequestDelay, err = dur("REQUEST_DELAY", "1s", true); err != nil {
		return Config{}, err
	}
	if c.Horizon, err = dur("HORIZON", "2160h", false); err != nil {
		return Config{}, err
	}
	if c.Retention, err = dur("RETENTION", "336h", true); err != nil {
		return Config{}, err
	}
	return c, nil
}
