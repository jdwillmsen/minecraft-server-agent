// Package config loads the agent's runtime configuration from environment
// variables. Every required variable fails fast with a clear error rather
// than falling back to a guess.
package config

import (
	"fmt"
	"net/url"
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

	// Postgres, for player profiles and playtime. An empty PGHost disables
	// persistence entirely: the agent then greets players the way it did
	// before Stage 3, which is a supported state rather than a degraded one.
	PGHost     string
	PGPort     int
	PGDatabase string
	PGUser     string
	PGPassword string
	// PGConnectTimeoutMs bounds the startup connection check. A database that
	// is slow to answer must not hold the agent out of the game: it connects,
	// answers commands and greets players without persistence.
	PGConnectTimeoutMs int

	// The LLM backend behind @server answering. An empty LLMBaseURL disables
	// answering entirely -- the mention is logged and nothing else happens,
	// which is the Stage 1-3 behaviour.
	LLMBaseURL   string
	LLMModel     string
	LLMAPIKey    string
	LLMMaxTokens int
	LLMTimeoutMs int
	// LLMTotalTimeoutMs bounds one whole answering attempt including every
	// tool round trip. Separate from LLMTimeoutMs, which bounds each
	// individual call: without the per-call bound one stalled request eats
	// the entire budget, and without this one a model that keeps calling
	// tools answers arbitrarily late.
	//
	// The two are not independent. One answer makes up to
	// (MaxToolRounds + 1) sequential calls -- three, for the two tool
	// rounds internal/adapters allows -- so this must be at least
	// (MaxToolRounds + 1) x LLMTimeoutMs, with margin for the tool calls
	// between them. Set below that and a model that uses both of its tool
	// rounds is cancelled before it ever answers, which the player who
	// asked experiences as silence.
	LLMTotalTimeoutMs int
	// AnswerMaxPerMinute bounds answers per player, separately from the
	// command limiter: one LLM call is far more expensive than one console
	// command, and a shared budget would let questions starve !help.
	AnswerMaxPerMinute int

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
	pgPort, err := positiveInt("PG_PORT", 5432)
	if err != nil {
		return Config{}, err
	}
	pgConnectTimeout, err := positiveInt("PG_CONNECT_TIMEOUT_MS", 5000)
	if err != nil {
		return Config{}, err
	}
	llmMaxTokens, err := positiveInt("LLM_MAX_TOKENS", 192)
	if err != nil {
		return Config{}, err
	}
	llmTimeout, err := positiveInt("LLM_TIMEOUT_MS", 8000)
	if err != nil {
		return Config{}, err
	}
	llmTotalTimeout, err := positiveInt("LLM_TOTAL_TIMEOUT_MS", 30000)
	if err != nil {
		return Config{}, err
	}
	answerRateLimit, err := positiveInt("ANSWER_MAX_PER_MINUTE", 4)
	if err != nil {
		return Config{}, err
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
		PGHost:                    stringDefault("PG_HOST", ""),
		PGPort:                    pgPort,
		PGDatabase:                stringDefault("PG_DATABASE", ""),
		PGUser:                    stringDefault("PG_USERNAME", ""),
		PGPassword:                stringDefault("PG_PASSWORD", ""),
		PGConnectTimeoutMs:        pgConnectTimeout,
		LLMBaseURL:                stringDefault("LLM_BASE_URL", ""),
		LLMModel:                  stringDefault("LLM_MODEL", ""),
		LLMAPIKey:                 stringDefault("LLM_API_KEY", ""),
		LLMMaxTokens:              llmMaxTokens,
		LLMTimeoutMs:              llmTimeout,
		LLMTotalTimeoutMs:         llmTotalTimeout,
		AnswerMaxPerMinute:        answerRateLimit,
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

// PostgresDSN assembles a connection string from the parts above.
//
// Assembled rather than taken whole so the password can arrive from a Secret
// reference on its own: a single DATABASE_URL would put the password into the
// Deployment's plain environment, readable by anyone who can get the object.
//
// url.UserPassword percent-encodes both halves, so a generated password
// containing a slash or an at-sign cannot silently produce a DSN that parses
// as a different host.
func (c Config) PostgresDSN() string {
	if c.PGHost == "" {
		return ""
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=require",
		url.UserPassword(c.PGUser, c.PGPassword).String(),
		c.PGHost, c.PGPort, c.PGDatabase)
}
