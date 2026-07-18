// Package config loads application configuration from environment variables
// with an optional .env file fallback. Real environment variables always take
// precedence over values defined in the .env file.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all configuration for the application. Values are populated
// from environment variables (and an optional .env file) via viper.
type Config struct {
	// AppEnv is the running environment, e.g. "development" or "production".
	AppEnv string `mapstructure:"APP_ENV"`
	// Host is the address the HTTP server binds to.
	Host string `mapstructure:"HOST"`
	// Port is the port the HTTP server listens on.
	Port string `mapstructure:"PORT"`
	// LogLevel controls verbosity, e.g. "debug", "info", "warn", "error".
	LogLevel string `mapstructure:"LOG_LEVEL"`
	// ServiceName is the logical service name reported in telemetry
	// (traces/metrics) as the OpenTelemetry `service.name` resource attribute.
	ServiceName string `mapstructure:"SERVICE_NAME"`
	// ServiceVersion is the service version reported in telemetry as the
	// OpenTelemetry `service.version` resource attribute.
	ServiceVersion string `mapstructure:"SERVICE_VERSION"`
	// ModelAccessKey is the secret credential used to authenticate with the
	// model provider. It has no default and must be supplied via the
	// environment (or .env file); it should never be committed to source.
	ModelAccessKey string `mapstructure:"MODEL_ACCESS_KEY"`
	// InferenceEndpoint is the upstream chat-completions URL that /v1/chat
	// proxies requests to.
	InferenceEndpoint string `mapstructure:"INFERENCE_ENDPOINT"`

	// Primary* tune the primary outbound HTTP client (request timeout and
	// connection-pool settings).
	PrimaryTimeout             time.Duration `mapstructure:"PRIMARY_TIMEOUT"`
	PrimaryMaxIdleConns        int           `mapstructure:"PRIMARY_MAX_IDLE_CONNS"`
	PrimaryMaxIdleConnsPerHost int           `mapstructure:"PRIMARY_MAX_IDLE_CONNS_PER_HOST"`
	PrimaryIdleConnTimeout     time.Duration `mapstructure:"PRIMARY_IDLE_CONN_TIMEOUT"`

	// Shadow* tune the shadow outbound HTTP client used for mirrored traffic.
	ShadowTimeout             time.Duration `mapstructure:"SHADOW_TIMEOUT"`
	ShadowMaxIdleConns        int           `mapstructure:"SHADOW_MAX_IDLE_CONNS"`
	ShadowMaxIdleConnsPerHost int           `mapstructure:"SHADOW_MAX_IDLE_CONNS_PER_HOST"`
	ShadowIdleConnTimeout     time.Duration `mapstructure:"SHADOW_IDLE_CONN_TIMEOUT"`
}

// Load reads configuration from environment variables and an optional .env
// file. Environment variables always take precedence over the file.
func Load() (Config, error) {
	v := viper.New()

	// Sensible defaults so the app runs with zero configuration.
	v.SetDefault("APP_ENV", "development")
	v.SetDefault("HOST", "0.0.0.0")
	v.SetDefault("PORT", "9090")
	v.SetDefault("LOG_LEVEL", "info")
	v.SetDefault("SERVICE_NAME", "llmeval")
	v.SetDefault("SERVICE_VERSION", "dev")
	// Secret with no default; must be provided via env or .env file.
	v.SetDefault("MODEL_ACCESS_KEY", "")
	v.SetDefault("INFERENCE_ENDPOINT", "https://inference.do-ai.run/v1/chat/completions")

	// Outbound HTTP client defaults (mirror httpx package defaults). Durations
	// accept Go duration strings (e.g. "10s", "90s").
	v.SetDefault("PRIMARY_TIMEOUT", "10s")
	v.SetDefault("PRIMARY_MAX_IDLE_CONNS", 100)
	v.SetDefault("PRIMARY_MAX_IDLE_CONNS_PER_HOST", 10)
	v.SetDefault("PRIMARY_IDLE_CONN_TIMEOUT", "90s")
	v.SetDefault("SHADOW_TIMEOUT", "10s")
	v.SetDefault("SHADOW_MAX_IDLE_CONNS", 100)
	v.SetDefault("SHADOW_MAX_IDLE_CONNS_PER_HOST", 10)
	v.SetDefault("SHADOW_IDLE_CONN_TIMEOUT", "90s")

	// Read from a .env file if present. Missing file is not an error.
	v.SetConfigName(".env")
	v.SetConfigType("env")
	v.AddConfigPath(".")
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return Config{}, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// Overlay real environment variables on top of file/defaults.
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return cfg, nil
}

// Addr returns the host:port string the server should listen on.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%s", c.Host, c.Port)
}
