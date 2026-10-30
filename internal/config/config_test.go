package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ListenAddr: ":8080", DataDir: "/data", APIToken: "", LogLevel: "info",
		RefreshInterval: 3 * time.Hour, RequestDelay: time.Second,
		Horizon: 2160 * time.Hour, Retention: 336 * time.Hour, RegLookups: 600, WatchInterval: time.Minute,
		MailFrom:       "Markham Drop-In <no-reply@vincentlfilms.com>",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LISTEN_ADDR": "127.0.0.1:9000", "DATA_DIR": "/tmp/x", "API_TOKEN": "s3cret",
		"LOG_LEVEL": "DEBUG", "REFRESH_INTERVAL": "30m", "REQUEST_DELAY": "0s",
		"HORIZON": "48h", "RETENTION": "0s", "REG_LOOKUPS": "50",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ListenAddr: "127.0.0.1:9000", DataDir: "/tmp/x", APIToken: "s3cret", LogLevel: "debug",
		RefreshInterval: 30 * time.Minute, RequestDelay: 0,
		Horizon: 48 * time.Hour, Retention: 0,
		RegLookups: 50, WatchInterval: time.Minute,
		MailFrom:       "Markham Drop-In <no-reply@vincentlfilms.com>",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v want %+v", c, want)
	}
}

func TestLoadBadValues(t *testing.T) {
	for _, tc := range []struct{ k, v string }{
		{"REFRESH_INTERVAL", "abc"},
		{"REQUEST_DELAY", "-1s"},
		{"HORIZON", "1x"},
		{"RETENTION", "-5h"},
		{"REFRESH_INTERVAL", "0s"},
		{"HORIZON", "0"},
		{"LOG_LEVEL", "loud"},
	} {
		_, err := Load(env(map[string]string{tc.k: tc.v}))
		if err == nil || !strings.Contains(err.Error(), tc.k) {
			t.Errorf("%s=%s: err = %v, want error naming %s", tc.k, tc.v, err, tc.k)
		}
	}
}

func TestLoadRegLookups(t *testing.T) {
	c, err := Load(env(map[string]string{"REG_LOOKUPS": "0"}))
	if err != nil || c.RegLookups != 0 {
		t.Fatalf("0 should be allowed (disables lookups): %+v %v", c, err)
	}
	for _, bad := range []string{"-1", "lots"} {
		if _, err := Load(env(map[string]string{"REG_LOOKUPS": bad})); err == nil || !strings.Contains(err.Error(), "REG_LOOKUPS") {
			t.Fatalf("%q: want error naming REG_LOOKUPS, got %v", bad, err)
		}
	}
}

func TestLoadAccountsConfig(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.MailFrom != "Markham Drop-In <no-reply@vincentlfilms.com>" || c.BaseURL != "" ||
		len(c.TrustedProxies) != 1 || c.TrustedProxies[0] != netip.MustParsePrefix("172.16.0.0/12") {
		t.Fatalf("defaults: %+v", c)
	}
	c, err = Load(env(map[string]string{
		"BASE_URL": "https://markham.example", "RESEND_API_KEY": "re_x", "MAIL_FROM": "Me <me@x.test>",
		"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET": "sec", "TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.0/24",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://markham.example" || c.ResendAPIKey != "re_x" || c.MailFrom != "Me <me@x.test>" ||
		c.TurnstileSiteKey != "site" || c.TurnstileSecret != "sec" ||
		len(c.TrustedProxies) != 2 || c.TrustedProxies[1] != netip.MustParsePrefix("192.168.1.0/24") {
		t.Fatalf("overrides: %+v", c)
	}
	_, err = Load(env(map[string]string{"TRUSTED_PROXIES": "nope"}))
	if err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Fatalf("bad proxies: %v", err)
	}
	for _, bad := range []string{"markham.example", "/path", "http://"} {
		_, err = Load(env(map[string]string{"BASE_URL": bad}))
		if err == nil || !strings.Contains(err.Error(), "BASE_URL") {
			t.Fatalf("BASE_URL %q: %v", bad, err)
		}
	}
}

func TestLoadTurnstileKeysArePaired(t *testing.T) {
	for _, m := range []map[string]string{
		{"TURNSTILE_SITE_KEY": "site"},
		{"TURNSTILE_SECRET": "sec"},
	} {
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), "TURNSTILE_SITE_KEY") ||
			!strings.Contains(err.Error(), "TURNSTILE_SECRET") {
			t.Errorf("%v: err = %v, want error naming both variables", m, err)
		}
	}
	c, err := Load(env(nil))
	if err != nil || c.TurnstileSiteKey != "" || c.TurnstileSecret != "" {
		t.Errorf("neither set = %+v, %v; want both empty", c, err)
	}
}

func TestLoadWatchInterval(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil || c.WatchInterval != time.Minute {
		t.Fatalf("default = %v, %v; want 1m", c.WatchInterval, err)
	}
	if _, err := Load(env(map[string]string{"WATCH_INTERVAL": "20s"})); err == nil || !strings.Contains(err.Error(), "WATCH_INTERVAL") {
		t.Fatalf("20s err = %v, want WATCH_INTERVAL error", err)
	}
	c, err = Load(env(map[string]string{"WATCH_INTERVAL": "45s"}))
	if err != nil || c.WatchInterval != 45*time.Second {
		t.Fatalf("45s = %v, %v", c.WatchInterval, err)
	}
}

func TestLoadAdminEmails(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_EMAILS": " A@x.com ,b@y.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a@x.com", "b@y.com"}; !reflect.DeepEqual(c.AdminEmails, want) {
		t.Fatalf("AdminEmails = %v, want %v", c.AdminEmails, want)
	}
	if _, err := Load(env(map[string]string{"ADMIN_EMAILS": "nope"})); err == nil || !strings.Contains(err.Error(), "ADMIN_EMAILS") {
		t.Fatalf("invalid err = %v, want ADMIN_EMAILS error", err)
	}
}
