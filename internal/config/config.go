// Package config loads the agent's runtime configuration from environment
// variables. Every required variable fails fast with a clear error rather
// than falling back to a guess.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the agent's fully-parsed runtime configuration.
type Config struct {
	// Bedrock connection.
	MCHost     string
	MCPort     int
	MCUsername string

	// Reconnect backoff, in milliseconds.
	ReconnectMinMs int
	ReconnectMaxMs int

	// HTTP server for /healthz and /metrics.
	HTTPAddr string

	// AuthCacheDir is where the Xbox Live device-code token is cached
	// across restarts. Must be a persistent volume in production.
	AuthCacheDir string

	// Logging.
	LogLevel string
}

// Load reads Config from the process environment.
func Load() (Config, error) {
	host, err := required("MC_HOST")
	if err != nil {
		return Config{}, err
	}
	username, err := required("MC_USERNAME")
	if err != nil {
		return Config{}, err
	}

	port, err := positiveInt("MC_PORT", 19132)
	if err != nil {
		return Config{}, err
	}
	reconnectMin, err := positiveInt("RECONNECT_MIN_MS", 5000)
	if err != nil {
		return Config{}, err
	}
	reconnectMax, err := positiveInt("RECONNECT_MAX_MS", 300000)
	if err != nil {
		return Config{}, err
	}
	if reconnectMax < reconnectMin {
		return Config{}, fmt.Errorf("RECONNECT_MAX_MS (%d) must be >= RECONNECT_MIN_MS (%d)", reconnectMax, reconnectMin)
	}

	cfg := Config{
		MCHost:         host,
		MCPort:         port,
		MCUsername:     username,
		ReconnectMinMs: reconnectMin,
		ReconnectMaxMs: reconnectMax,
		HTTPAddr:       stringDefault("HTTP_ADDR", ":8080"),
		AuthCacheDir:   stringDefault("AUTH_CACHE_DIR", "/data/auth"),
		LogLevel:       strings.ToLower(stringDefault("LOG_LEVEL", "info")),
	}
	return cfg, nil
}

func required(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}
	return v, nil
}

func stringDefault(name, def string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	return v
}

func positiveInt(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be an integer, got %q", name, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("environment variable %s must be a positive integer, got %d", name, n)
	}
	return n, nil
}
