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

	// HTTP server for /healthz, /readyz, and /metrics.
	HTTPAddr string

	// AuthCacheDir is where the Xbox Live device-code token is cached
	// across restarts. Must be a persistent volume in production.
	AuthCacheDir string

	// CommandRateLimitPerMinute caps how many ! commands a single actor
	// (XUID) may trigger per rolling minute, so chat spam can't turn into
	// unbounded downstream calls once a command's Run does real work.
	CommandRateLimitPerMinute int

	// ConsoleBridgeURL is the base URL of mc-console-bridge's HTTP API
	// (e.g. http://<release>-console-bridge.<ns>.svc.cluster.local:8766).
	// This is the only path to server-voice output and permissions.json.
	ConsoleBridgeURL string
	// ConsoleBridgeToken authenticates every bridge request as a bearer
	// token; the bridge rejects anything else.
	ConsoleBridgeToken string
	// MCMonitorURL and BackupExporterURL are the Prometheus exposition
	// endpoints behind !online, !version and !backup. Both are optional:
	// unset means those commands report themselves unconfigured rather than
	// erroring, so an agent can run without the exporters.
	//
	// Note the backup exporter serves /metrics.txt, not /metrics -- the full
	// path belongs in the value, not assembled here, so a future exporter with
	// a different path needs no code change.
	MCMonitorURL      string
	BackupExporterURL string

	// ConsoleBridgeTimeoutMs bounds every individual bridge HTTP call.
	ConsoleBridgeTimeoutMs int

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

	port, err := portNumber("MC_PORT", 19132)
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
	commandRateLimit, err := positiveInt("COMMAND_RATE_LIMIT_PER_MINUTE", 10)
	if err != nil {
		return Config{}, err
	}
	bridgeURL, err := required("CONSOLE_BRIDGE_URL")
	if err != nil {
		return Config{}, err
	}
	bridgeToken, err := required("CONSOLE_BRIDGE_TOKEN")
	if err != nil {
		return Config{}, err
	}
	bridgeTimeout, err := positiveInt("CONSOLE_BRIDGE_TIMEOUT_MS", 5000)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		MCHost:                    host,
		MCPort:                    port,
		MCUsername:                username,
		ReconnectMinMs:            reconnectMin,
		ReconnectMaxMs:            reconnectMax,
		HTTPAddr:                  stringDefault("HTTP_ADDR", ":8080"),
		AuthCacheDir:              stringDefault("AUTH_CACHE_DIR", "/data/auth"),
		MCMonitorURL:              stringDefault("MC_MONITOR_URL", ""),
		BackupExporterURL:         stringDefault("BACKUP_EXPORTER_URL", ""),
		CommandRateLimitPerMinute: commandRateLimit,
		ConsoleBridgeURL:          bridgeURL,
		ConsoleBridgeToken:        bridgeToken,
		ConsoleBridgeTimeoutMs:    bridgeTimeout,
		LogLevel:                  strings.ToLower(stringDefault("LOG_LEVEL", "info")),
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

// portNumber is positiveInt narrowed to the valid TCP/UDP port range, so a
// typo like MC_PORT=99999999 fails at startup instead of surfacing later as
// an inscrutable dial error.
func portNumber(name string, def int) (int, error) {
	n, err := positiveInt(name, def)
	if err != nil {
		return 0, err
	}
	if n > 65535 {
		return 0, fmt.Errorf("environment variable %s must be a valid port (1-65535), got %d", name, n)
	}
	return n, nil
}
