package config

import (
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
		Horizon: 2160 * time.Hour, Retention: 336 * time.Hour, RegLookups: 600,
	}
	if c != want {
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
		RegLookups: 50,
	}
	if c != want {
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
