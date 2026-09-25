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
	// AuthRetryDelayMs is the floor waited after Xbox Live rejects the
	// account itself rather than kicking the session for an ordinary
	// reason. Retrying at ReconnectMaxMs only feeds more failed logins to
	// whatever hold the account is under, which is what stretched a real
	// rejection out over dozens of attempts; this default is long enough to
	// plausibly outlast that hold instead.
	AuthRetryDelayMs int

	// SessionRecycleMs makes the agent drop and re-establish its Bedrock
	// session on a schedule. Zero, the default, leaves the session alone.
	//
	// It exists as a check rather than as hygiene. Every reachability check
	// this cluster runs answers "does the server respond", and all of them
	// passed for the whole 2026-09-15 outage while nobody could join; a
	// client already holding a session could not have noticed either, since
	// the sessions that mattered were established before the fault. Dropping
	// this one on purpose turns the agent into the only thing that
	// periodically proves a real account can still get all the way to spawn.
	//
	// The cost is real and bounded: the agent leaves chat for the length of
	// a reconnect, which is the same gap a deployment already causes.
	SessionRecycleMs int

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

	// LeaderPollMs is how often a standby asks whether the agent lock has come
	// free. It is the dominant term in how long a release leaves the server
	// without an agent: the departing process releases the lock as it goes,
	// and nobody plays again until a standby notices. Only meaningful with a
	// database configured -- without one there is no lock and no standby.
	LeaderPollMs int
	// LeaderMaxWaitMs bounds that wait, after which the agent goes live
	// without the lock.
	//
	// The lock is released by the connection holding it ending, which is
	// instant for a process that exits and hours for a pod that died without
	// closing its socket: PostgreSQL keeps that backend, and its lock, until
	// TCP keepalive reaps it. Waiting that out would cost the server its agent
	// for far longer than the gap this whole mechanism exists to shorten, so
	// the wait ends here and the Xbox Live kick evicts whatever is still
	// logged in -- exactly what a restart did before there was a lock.
	//
	// Must stay comfortably above LeaderPollMs and above the few seconds a
	// departing agent spends leaving the game and settling its database, so no
	// ordinary release ever reaches it.
	//
	// It is a floor rather than a deadline: a standby that can still hear the
	// holder announcing itself keeps waiting past it, since the point of the
	// bound is a lock nobody will release, not a lock somebody is using.
	LeaderMaxWaitMs int
	// LeaderHeartbeatMs is how often the live agent announces that it is still
	// there, which is the only thing that tells a standby a slow holder from a
	// dead one.
	//
	// Must be under LeaderMaxWaitMs or a standby goes live without ever having
	// had the chance to hear one, and is worth keeping under a third of it: a
	// standby waits three intervals of silence before it stops believing in the
	// holder, and while that is shorter than the bound, a holder that really is
	// gone costs a standby the bound and nothing more.
	LeaderHeartbeatMs int

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

	// ModerationTerms are the words whose use in public chat is recorded,
	// from a comma-separated MODERATION_TERMS. Empty turns the term rule off.
	// The other rules need no configuration.
	ModerationTerms []string

	// AnnounceAPIToken is the bearer token POST /announcements requires.
	// Unset leaves the route unmounted, answering 404 like any path that
	// was never there: a disabled API must be indistinguishable from an
	// absent one, and never open.
	AnnounceAPIToken string

	// PresenceActors is every account that puts a player into the world, from
	// PRESENCE_ACTORS. Empty turns presence control off entirely: the agent is
	// always in the world, as before it existed.
	PresenceActors []PresenceActor
	// PresenceTokens authorise the /v1 presence routes. Empty leaves them
	// unmounted, on the same terms as AnnounceAPIToken.
	PresenceTokens []PresenceToken
	// PresenceSelfID is which of PresenceActors this process is.
	PresenceSelfID string

	// MaxToolRounds caps tool rounds per answer. Load refuses a value the
	// timeouts cannot honour: (MaxToolRounds + 1) sequential calls must fit
	// inside LLMTotalTimeoutMs, or a model that uses its budget is cancelled
	// before it answers.
	MaxToolRounds int
	// WikiEnabled registers wiki_lookup. WikiBaseURL is pinned to
	// WikiProductionURL unless WIKI_ALLOW_TEST_BASE_URL is set, so a stray
	// value cannot point the agent at a host an operator never chose.
	WikiEnabled bool
	WikiBaseURL string

	// Logging.
	LogLevel string
}

// WikiProductionURL is the only wiki API the agent is meant to call.
const WikiProductionURL = "https://minecraft.wiki/api.php"

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
	authRetryDelay, err := positiveInt("AUTH_RETRY_DELAY_MS", 900000)
	if err != nil {
		return Config{}, err
	}
	sessionRecycle, err := nonNegativeInt("SESSION_RECYCLE_MS", 0)
	if err != nil {
		return Config{}, err
	}
	// A recycle that fires faster than the reconnect ladder can settle would
	// spend the agent's life reconnecting, which costs chat presence and
	// proves nothing a slower cadence does not. Ten ceilings is 50 minutes at
	// the defaults, and the failure this looks for is measured in hours of
	// nobody being able to join -- so the floor costs the check nothing it
	// would otherwise catch.
	if sessionRecycle > 0 && sessionRecycle < 10*reconnectMax {
		return Config{}, fmt.Errorf("SESSION_RECYCLE_MS (%d) must be at least ten times RECONNECT_MAX_MS (%d) or zero", sessionRecycle, reconnectMax)
	}
	pgPort, err := positiveInt("PG_PORT", 5432)
	if err != nil {
		return Config{}, err
	}
	pgConnectTimeout, err := positiveInt("PG_CONNECT_TIMEOUT_MS", 5000)
	if err != nil {
		return Config{}, err
	}
	leaderPoll, err := positiveInt("LEADER_POLL_MS", 500)
	if err != nil {
		return Config{}, err
	}
	leaderMaxWait, err := positiveInt("LEADER_MAX_WAIT_MS", 60000)
	if err != nil {
		return Config{}, err
	}
	if leaderMaxWait < leaderPoll {
		// A bound below one poll interval is a bound that gives up before it
		// has asked twice, which is not a fallback for a lock nobody will
		// release -- it is the lock turned off, by a value that looks like
		// tuning.
		return Config{}, fmt.Errorf("LEADER_MAX_WAIT_MS (%d) must be >= LEADER_POLL_MS (%d)", leaderMaxWait, leaderPoll)
	}
	leaderHeartbeat, err := positiveInt("LEADER_HEARTBEAT_MS", 10000)
	if err != nil {
		return Config{}, err
	}
	if leaderHeartbeat >= leaderMaxWait {
		// An announcement that comes round less often than the bound is one no
		// standby ever hears in time: it would go live on a silence that meant
		// only that the holder had not got round to speaking yet.
		return Config{}, fmt.Errorf("LEADER_HEARTBEAT_MS (%d) must be < LEADER_MAX_WAIT_MS (%d)", leaderHeartbeat, leaderMaxWait)
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
	maxToolRounds, err := positiveInt("MAX_TOOL_ROUNDS", 2)
	if err != nil {
		return Config{}, err
	}
	if maxToolRounds > 6 {
		return Config{}, fmt.Errorf("MAX_TOOL_ROUNDS must be between 1 and 6, got %d", maxToolRounds)
	}
	if need := (maxToolRounds + 1) * llmTimeout; llmTotalTimeout < need {
		return Config{}, fmt.Errorf("LLM_TOTAL_TIMEOUT_MS (%d) must be at least (MAX_TOOL_ROUNDS + 1) x LLM_TIMEOUT_MS = %d", llmTotalTimeout, need)
	}
	wikiEnabled, err := boolDefault("WIKI_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	allowTestWiki, err := boolDefault("WIKI_ALLOW_TEST_BASE_URL", false)
	if err != nil {
		return Config{}, err
	}
	wikiBaseURL := stringDefault("WIKI_BASE_URL", WikiProductionURL)
	if wikiBaseURL != WikiProductionURL && !allowTestWiki {
		return Config{}, fmt.Errorf("WIKI_BASE_URL must be %s unless WIKI_ALLOW_TEST_BASE_URL=true, got %q", WikiProductionURL, wikiBaseURL)
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
	presenceActors, presenceTokens, presenceSelf, err := loadPresence()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		MCHost:                    host,
		MCPort:                    port,
		MCUsername:                username,
		ReconnectMinMs:            reconnectMin,
		ReconnectMaxMs:            reconnectMax,
		AuthRetryDelayMs:          authRetryDelay,
		SessionRecycleMs:          sessionRecycle,
		HTTPAddr:                  stringDefault("HTTP_ADDR", ":8080"),
		AuthCacheDir:              stringDefault("AUTH_CACHE_DIR", "/data/auth"),
		PGHost:                    stringDefault("PG_HOST", ""),
		PGPort:                    pgPort,
		PGDatabase:                stringDefault("PG_DATABASE", ""),
		PGUser:                    stringDefault("PG_USERNAME", ""),
		PGPassword:                stringDefault("PG_PASSWORD", ""),
		PGConnectTimeoutMs:        pgConnectTimeout,
		LeaderPollMs:              leaderPoll,
		LeaderMaxWaitMs:           leaderMaxWait,
		LeaderHeartbeatMs:         leaderHeartbeat,
		LLMBaseURL:                stringDefault("LLM_BASE_URL", ""),
		LLMModel:                  stringDefault("LLM_MODEL", ""),
		LLMAPIKey:                 stringDefault("LLM_API_KEY", ""),
		LLMMaxTokens:              llmMaxTokens,
		LLMTimeoutMs:              llmTimeout,
		LLMTotalTimeoutMs:         llmTotalTimeout,
		MaxToolRounds:             maxToolRounds,
		WikiEnabled:               wikiEnabled,
		WikiBaseURL:               wikiBaseURL,
		AnswerMaxPerMinute:        answerRateLimit,
		MCMonitorURL:              stringDefault("MC_MONITOR_URL", ""),
		BackupExporterURL:         stringDefault("BACKUP_EXPORTER_URL", ""),
		CommandRateLimitPerMinute: commandRateLimit,
		ConsoleBridgeURL:          bridgeURL,
		ConsoleBridgeToken:        bridgeToken,
		ConsoleBridgeTimeoutMs:    bridgeTimeout,
		ModerationTerms:           commaList("MODERATION_TERMS"),
		AnnounceAPIToken:          stringDefault("ANNOUNCE_API_TOKEN", ""),
		PresenceActors:            presenceActors,
		PresenceTokens:            presenceTokens,
		PresenceSelfID:            presenceSelf,
		LogLevel:                  strings.ToLower(stringDefault("LOG_LEVEL", "info")),
	}
	return cfg, nil
}

// commaList splits a comma-separated variable, trimming each entry and
// dropping blanks. A trailing comma or a doubled comma is then just
// punctuation, not an empty entry for every consumer to guard against.
func commaList(name string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(name), ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// boolDefault reads a boolean the way strconv does ("true", "1", "false",
// "0", ...) and rejects anything else rather than reading it as false: a
// typo in an enable flag should stop the process, not quietly disable the
// feature.
func boolDefault(name string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("environment variable %s must be true or false, got %q", name, raw)
	}
	return v, nil
}

// nonNegativeInt is positiveInt's sibling for settings where zero is a real
// value rather than a mistake -- an interval of zero means "never", which is
// how an optional periodic behaviour is switched off.
func nonNegativeInt(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s must be an integer, got %q", name, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("environment variable %s must not be negative, got %d", name, n)
	}
	return n, nil
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
