// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr, DataDir, APIToken, LogLevel string
	RefreshInterval, RequestDelay           time.Duration
	Horizon, Retention                      time.Duration
	// RegLookups caps registration-page fetches per refresh; 0 disables them.
	RegLookups int

	// BaseURL prefixes links in emails; empty means main picks a local default.
	BaseURL string
	// ResendAPIKey enables sending email through Resend; MailFrom is the sender.
	ResendAPIKey, MailFrom string
	// TurnstileSiteKey and TurnstileSecret enable Cloudflare Turnstile.
	TurnstileSiteKey, TurnstileSecret string
	// TrustedProxies are the proxy networks whose forwarded headers are believed.
	TrustedProxies []netip.Prefix
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

		BaseURL:          getenv("BASE_URL"),
		ResendAPIKey:     getenv("RESEND_API_KEY"),
		MailFrom:         str("MAIL_FROM", "Markham Drop-In <no-reply@vincentlfilms.com>"),
		TurnstileSiteKey: getenv("TURNSTILE_SITE_KEY"),
		TurnstileSecret:  getenv("TURNSTILE_SECRET"),
	}
	if c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("BASE_URL: invalid value %q (want a full URL such as https://markham.example)", c.BaseURL)
		}
	}
	for _, part := range strings.Split(str("TRUSTED_PROXIES", "172.16.0.0/12"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return Config{}, fmt.Errorf("TRUSTED_PROXIES: invalid CIDR %q", part)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
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
	c.RegLookups = 600
	if v := getenv("REG_LOOKUPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("REG_LOOKUPS: invalid value %q (want a whole number, 0 to disable)", v)
		}
		c.RegLookups = n
	}
	return c, nil
}
