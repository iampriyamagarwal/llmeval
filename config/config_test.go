package config

import (
	"os"
	"testing"
	"time"
)

var configKeys = []string{
	"APP_ENV", "HOST", "PORT", "LOG_LEVEL", "SERVICE_NAME", "SERVICE_VERSION",
	"MODEL_ACCESS_KEY", "INFERENCE_ENDPOINT",
	"PRIMARY_TIMEOUT", "PRIMARY_MAX_IDLE_CONNS", "PRIMARY_MAX_IDLE_CONNS_PER_HOST",
	"PRIMARY_IDLE_CONN_TIMEOUT",
	"SHADOW_TIMEOUT", "SHADOW_MAX_IDLE_CONNS", "SHADOW_MAX_IDLE_CONNS_PER_HOST",
	"SHADOW_IDLE_CONN_TIMEOUT",
	"SERVER_READ_TIMEOUT", "SERVER_WRITE_TIMEOUT", "SERVER_IDLE_TIMEOUT",
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

	if cfg.InferenceEndpoint != "https://inference.do-ai.run/v1/chat/completions" {
		t.Errorf("InferenceEndpoint = %q, want the DO inference URL", cfg.InferenceEndpoint)
	}

	// Outbound client defaults mirror the httpx package defaults.
	if cfg.PrimaryTimeout != 10*time.Second {
		t.Errorf("PrimaryTimeout = %v, want 10s", cfg.PrimaryTimeout)
	}
	if cfg.PrimaryMaxIdleConns != 100 {
		t.Errorf("PrimaryMaxIdleConns = %d, want 100", cfg.PrimaryMaxIdleConns)
	}
	if cfg.PrimaryMaxIdleConnsPerHost != 10 {
		t.Errorf("PrimaryMaxIdleConnsPerHost = %d, want 10", cfg.PrimaryMaxIdleConnsPerHost)
	}
	if cfg.PrimaryIdleConnTimeout != 90*time.Second {
		t.Errorf("PrimaryIdleConnTimeout = %v, want 90s", cfg.PrimaryIdleConnTimeout)
	}
	if cfg.ShadowTimeout != 10*time.Second {
		t.Errorf("ShadowTimeout = %v, want 10s", cfg.ShadowTimeout)
	}
	if cfg.ShadowMaxIdleConns != 100 {
		t.Errorf("ShadowMaxIdleConns = %d, want 100", cfg.ShadowMaxIdleConns)
	}
	if cfg.ShadowMaxIdleConnsPerHost != 10 {
		t.Errorf("ShadowMaxIdleConnsPerHost = %d, want 10", cfg.ShadowMaxIdleConnsPerHost)
	}
	if cfg.ShadowIdleConnTimeout != 90*time.Second {
		t.Errorf("ShadowIdleConnTimeout = %v, want 90s", cfg.ShadowIdleConnTimeout)
	}

	// HTTP server timeout defaults.
	if cfg.ServerReadTimeout != 10*time.Second {
		t.Errorf("ServerReadTimeout = %v, want 10s", cfg.ServerReadTimeout)
	}
	if cfg.ServerWriteTimeout != 10*time.Second {
		t.Errorf("ServerWriteTimeout = %v, want 10s", cfg.ServerWriteTimeout)
	}
	if cfg.ServerIdleTimeout != 60*time.Second {
		t.Errorf("ServerIdleTimeout = %v, want 60s", cfg.ServerIdleTimeout)
	}
}

func TestLoadClientEnvOverrides(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("PRIMARY_TIMEOUT", "5s")
	t.Setenv("PRIMARY_MAX_IDLE_CONNS", "250")
	t.Setenv("PRIMARY_MAX_IDLE_CONNS_PER_HOST", "50")
	t.Setenv("PRIMARY_IDLE_CONN_TIMEOUT", "30s")
	t.Setenv("SHADOW_TIMEOUT", "2s")
	t.Setenv("SHADOW_MAX_IDLE_CONNS", "20")
	t.Setenv("SHADOW_MAX_IDLE_CONNS_PER_HOST", "5")
	t.Setenv("SHADOW_IDLE_CONN_TIMEOUT", "15s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.PrimaryTimeout != 5*time.Second {
		t.Errorf("PrimaryTimeout = %v, want 5s", cfg.PrimaryTimeout)
	}
	if cfg.PrimaryMaxIdleConns != 250 {
		t.Errorf("PrimaryMaxIdleConns = %d, want 250", cfg.PrimaryMaxIdleConns)
	}
	if cfg.PrimaryMaxIdleConnsPerHost != 50 {
		t.Errorf("PrimaryMaxIdleConnsPerHost = %d, want 50", cfg.PrimaryMaxIdleConnsPerHost)
	}
	if cfg.PrimaryIdleConnTimeout != 30*time.Second {
		t.Errorf("PrimaryIdleConnTimeout = %v, want 30s", cfg.PrimaryIdleConnTimeout)
	}
	if cfg.ShadowTimeout != 2*time.Second {
		t.Errorf("ShadowTimeout = %v, want 2s", cfg.ShadowTimeout)
	}
	if cfg.ShadowMaxIdleConns != 20 {
		t.Errorf("ShadowMaxIdleConns = %d, want 20", cfg.ShadowMaxIdleConns)
	}
	if cfg.ShadowMaxIdleConnsPerHost != 5 {
		t.Errorf("ShadowMaxIdleConnsPerHost = %d, want 5", cfg.ShadowMaxIdleConnsPerHost)
	}
	if cfg.ShadowIdleConnTimeout != 15*time.Second {
		t.Errorf("ShadowIdleConnTimeout = %v, want 15s", cfg.ShadowIdleConnTimeout)
	}
}

func TestLoadServerTimeoutEnvOverrides(t *testing.T) {
	clearConfigEnv(t)

	t.Setenv("SERVER_READ_TIMEOUT", "15s")
	t.Setenv("SERVER_WRITE_TIMEOUT", "20s")
	t.Setenv("SERVER_IDLE_TIMEOUT", "120s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.ServerReadTimeout != 15*time.Second {
		t.Errorf("ServerReadTimeout = %v, want 15s", cfg.ServerReadTimeout)
	}
	if cfg.ServerWriteTimeout != 20*time.Second {
		t.Errorf("ServerWriteTimeout = %v, want 20s", cfg.ServerWriteTimeout)
	}
	if cfg.ServerIdleTimeout != 120*time.Second {
		t.Errorf("ServerIdleTimeout = %v, want 120s", cfg.ServerIdleTimeout)
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
