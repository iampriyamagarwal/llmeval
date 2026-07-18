package config

import (
	"os"
	"testing"
)

var configKeys = []string{
	"APP_ENV", "HOST", "PORT", "LOG_LEVEL", "SERVICE_NAME", "SERVICE_VERSION",
}

// clearConfigEnv unsets all config env vars for the duration of the test,
// restoring their original values afterwards.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range configKeys {
		orig, had := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		if had {
			key := key
			orig := orig
			t.Cleanup(func() { _ = os.Setenv(key, orig) })
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	got := map[string]string{
		"APP_ENV":         cfg.AppEnv,
		"HOST":            cfg.Host,
		"PORT":            cfg.Port,
		"LOG_LEVEL":       cfg.LogLevel,
		"SERVICE_NAME":    cfg.ServiceName,
		"SERVICE_VERSION": cfg.ServiceVersion,
	}
	want := map[string]string{
		"APP_ENV":         "development",
		"HOST":            "0.0.0.0",
		"PORT":            "9090",
		"LOG_LEVEL":       "info",
		"SERVICE_NAME":    "llmeval",
		"SERVICE_VERSION": "dev",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("APP_ENV", "production")
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("PORT", "8080")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SERVICE_NAME", "custom-svc")
	t.Setenv("SERVICE_VERSION", "1.2.3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.AppEnv != "production" {
		t.Errorf("AppEnv = %q, want production", cfg.AppEnv)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.ServiceName != "custom-svc" {
		t.Errorf("ServiceName = %q, want custom-svc", cfg.ServiceName)
	}
	if cfg.ServiceVersion != "1.2.3" {
		t.Errorf("ServiceVersion = %q, want 1.2.3", cfg.ServiceVersion)
	}
}

func TestAddr(t *testing.T) {
	cfg := Config{Host: "0.0.0.0", Port: "9090"}
	if got, want := cfg.Addr(), "0.0.0.0:9090"; got != want {
		t.Errorf("Addr() = %q, want %q", got, want)
	}
}
