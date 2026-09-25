package config

import (
	"os"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MC_HOST", "MC_USERNAME", "MC_PORT",
		"RECONNECT_MIN_MS", "RECONNECT_MAX_MS", "AUTH_RETRY_DELAY_MS", "SESSION_RECYCLE_MS",
		"HTTP_ADDR", "AUTH_CACHE_DIR",
		"COMMAND_RATE_LIMIT_PER_MINUTE", "LOG_LEVEL",
		"CONSOLE_BRIDGE_URL", "CONSOLE_BRIDGE_TOKEN", "CONSOLE_BRIDGE_TIMEOUT_MS",
		"LLM_MAX_TOKENS", "LLM_TIMEOUT_MS", "LLM_TOTAL_TIMEOUT_MS",
		"LEADER_POLL_MS", "LEADER_MAX_WAIT_MS", "LEADER_HEARTBEAT_MS",
		"MAX_TOOL_ROUNDS", "WIKI_ENABLED", "WIKI_BASE_URL", "WIKI_ALLOW_TEST_BASE_URL",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// setRequired sets every env var Load() requires to succeed, so tests that
// aren't exercising one of these specifically don't need to restate them.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("CONSOLE_BRIDGE_URL", "http://bridge.example.internal:8766")
	t.Setenv("CONSOLE_BRIDGE_TOKEN", "test-token")
}

func TestLoad_MissingHostFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_HOST", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when MC_HOST is unset")
	}
}

func TestLoad_MissingUsernameFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_USERNAME", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when MC_USERNAME is unset")
	}
}

func TestLoad_MissingConsoleBridgeURLFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("CONSOLE_BRIDGE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when CONSOLE_BRIDGE_URL is unset")
	}
}

func TestLoad_MissingConsoleBridgeTokenFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("CONSOLE_BRIDGE_TOKEN", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when CONSOLE_BRIDGE_TOKEN is unset")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCPort != 19132 {
		t.Errorf("MCPort = %d, want 19132", cfg.MCPort)
	}
	if cfg.ReconnectMinMs != 5000 {
		t.Errorf("ReconnectMinMs = %d, want 5000", cfg.ReconnectMinMs)
	}
	if cfg.ReconnectMaxMs != 300000 {
		t.Errorf("ReconnectMaxMs = %d, want 300000", cfg.ReconnectMaxMs)
	}
	if cfg.AuthRetryDelayMs != 900000 {
		t.Errorf("AuthRetryDelayMs = %d, want 900000", cfg.AuthRetryDelayMs)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.AuthCacheDir != "/data/auth" {
		t.Errorf("AuthCacheDir = %q, want /data/auth", cfg.AuthCacheDir)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.CommandRateLimitPerMinute != 10 {
		t.Errorf("CommandRateLimitPerMinute = %d, want 10", cfg.CommandRateLimitPerMinute)
	}
	if cfg.ConsoleBridgeURL != "http://bridge.example.internal:8766" {
		t.Errorf("ConsoleBridgeURL = %q, want the configured URL", cfg.ConsoleBridgeURL)
	}
	if cfg.ConsoleBridgeToken != "test-token" {
		t.Errorf("ConsoleBridgeToken = %q, want the configured token", cfg.ConsoleBridgeToken)
	}
	if cfg.ConsoleBridgeTimeoutMs != 5000 {
		t.Errorf("ConsoleBridgeTimeoutMs = %d, want 5000", cfg.ConsoleBridgeTimeoutMs)
	}
	if cfg.LeaderPollMs != 500 {
		t.Errorf("LeaderPollMs = %d, want 500", cfg.LeaderPollMs)
	}
	if cfg.LeaderMaxWaitMs != 60000 {
		t.Errorf("LeaderMaxWaitMs = %d, want 60000", cfg.LeaderMaxWaitMs)
	}
}

// The bound is what stops a lock nobody will release becoming an outage, so a
// value that gives up before the first poll has even been repeated is refused
// rather than quietly treated as "no lock".
func TestLoad_LeaderMaxWaitBelowThePollIntervalFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_POLL_MS", "5000")
	t.Setenv("LEADER_MAX_WAIT_MS", "1000")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when LEADER_MAX_WAIT_MS is below LEADER_POLL_MS")
	}
}

func TestLoad_LeaderMaxWaitOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_MAX_WAIT_MS", "120000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LeaderMaxWaitMs != 120000 {
		t.Errorf("LeaderMaxWaitMs = %d, want 120000", cfg.LeaderMaxWaitMs)
	}
}

// The standby's poll interval is most of the gap a release leaves, so it is
// worth being able to tune without a rebuild -- and worth refusing outright
// when it is set to something that would mean never asking.
func TestLoad_LeaderPollOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_POLL_MS", "250")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LeaderPollMs != 250 {
		t.Errorf("LeaderPollMs = %d, want 250", cfg.LeaderPollMs)
	}
}

func TestLoad_LeaderPollZeroFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_POLL_MS", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when LEADER_POLL_MS is 0")
	}
}

func TestLoad_LeaderHeartbeatOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_HEARTBEAT_MS", "4000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LeaderHeartbeatMs != 4000 {
		t.Errorf("LeaderHeartbeatMs = %d, want 4000", cfg.LeaderHeartbeatMs)
	}
}

// An agent that announces itself less often than a standby is willing to wait
// is an agent no standby ever hears in time: it would go live on a silence
// that only meant the holder had not got round to speaking yet.
func TestLoad_LeaderHeartbeatAboveTheBoundFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("LEADER_MAX_WAIT_MS", "10000")
	t.Setenv("LEADER_HEARTBEAT_MS", "10000")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when LEADER_HEARTBEAT_MS is not below LEADER_MAX_WAIT_MS")
	}
}

func TestLoad_ConsoleBridgeTimeoutOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("CONSOLE_BRIDGE_TIMEOUT_MS", "1500")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConsoleBridgeTimeoutMs != 1500 {
		t.Errorf("ConsoleBridgeTimeoutMs = %d, want 1500", cfg.ConsoleBridgeTimeoutMs)
	}
}

func TestLoad_ConsoleBridgeTimeoutZeroFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("CONSOLE_BRIDGE_TIMEOUT_MS", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for a zero CONSOLE_BRIDGE_TIMEOUT_MS")
	}
}

func TestLoad_PortOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_PORT", "25565")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCPort != 25565 {
		t.Errorf("MCPort = %d, want 25565", cfg.MCPort)
	}
}

func TestLoad_NonIntegerPortFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_PORT", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-integer MC_PORT")
	}
}

func TestLoad_ZeroPortFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_PORT", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for zero MC_PORT")
	}
}

func TestLoad_PortAboveValidRangeFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_PORT", "99999999")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for MC_PORT above 65535")
	}
}

func TestLoad_MaxValidPortSucceeds(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_PORT", "65535")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCPort != 65535 {
		t.Errorf("MCPort = %d, want 65535", cfg.MCPort)
	}
}

func TestLoad_SessionRecycleIsOffByDefault(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Off by default on purpose: the recycle drops a working session, and a
	// release should not start doing that to production because it was
	// deployed.
	if cfg.SessionRecycleMs != 0 {
		t.Errorf("SessionRecycleMs = %d, want 0", cfg.SessionRecycleMs)
	}
}

func TestLoad_SessionRecycleOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("SESSION_RECYCLE_MS", "21600000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionRecycleMs != 21600000 {
		t.Errorf("SessionRecycleMs = %d, want 21600000", cfg.SessionRecycleMs)
	}
}

// A recycle faster than the reconnect ladder can settle spends the agent's
// life reconnecting, which costs chat presence and measures nothing extra.
func TestLoad_SessionRecycleBelowTheFloorFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("RECONNECT_MAX_MS", "300000")
	t.Setenv("SESSION_RECYCLE_MS", "60000")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error when SESSION_RECYCLE_MS is under ten reconnect ceilings")
	}
}

func TestLoad_ReconnectMaxBelowMinFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("RECONNECT_MIN_MS", "10000")
	t.Setenv("RECONNECT_MAX_MS", "1000")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when RECONNECT_MAX_MS < RECONNECT_MIN_MS")
	}
}

func TestLoad_AuthRetryDelayOverride(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("AUTH_RETRY_DELAY_MS", "60000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthRetryDelayMs != 60000 {
		t.Errorf("AuthRetryDelayMs = %d, want 60000", cfg.AuthRetryDelayMs)
	}
}

func TestLoad_AuthRetryDelayZeroFails(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("AUTH_RETRY_DELAY_MS", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for a zero AUTH_RETRY_DELAY_MS")
	}
}

func TestLoad_HostAndUsernameAreTrimmed(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MC_HOST", "  mc.example.internal  ")
	t.Setenv("MC_USERNAME", "  agent-bot  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCHost != "mc.example.internal" {
		t.Errorf("MCHost = %q, want trimmed", cfg.MCHost)
	}
	if cfg.MCUsername != "agent-bot" {
		t.Errorf("MCUsername = %q, want trimmed", cfg.MCUsername)
	}
}

func TestLoad_LLMAnswerBudgetDefaults(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// At least (MaxToolRounds + 1) x LLM_TIMEOUT_MS, with margin: three
	// sequential 8s calls fit in the shipped default, and a model that uses
	// both tool rounds still gets to answer.
	if cfg.LLMTotalTimeoutMs != 30000 {
		t.Errorf("LLMTotalTimeoutMs = %d, want 30000", cfg.LLMTotalTimeoutMs)
	}
	if cfg.LLMMaxTokens != 192 {
		t.Errorf("LLMMaxTokens = %d, want 192 -- 96 cannot hold tool arguments plus an answer", cfg.LLMMaxTokens)
	}
}

func TestLoad_ModerationTermsAreTrimmedAndBlanksDropped(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MODERATION_TERMS", " griefer, ,Free Diamonds,,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"griefer", "Free Diamonds"}
	if len(cfg.ModerationTerms) != len(want) {
		t.Fatalf("ModerationTerms = %q, want %q", cfg.ModerationTerms, want)
	}
	for i := range want {
		if cfg.ModerationTerms[i] != want[i] {
			t.Errorf("ModerationTerms[%d] = %q, want %q", i, cfg.ModerationTerms[i], want[i])
		}
	}
}

func TestLoad_NoModerationTermsTurnsTheRuleOff(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MODERATION_TERMS", "")
	os.Unsetenv("MODERATION_TERMS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModerationTerms) != 0 {
		t.Errorf("ModerationTerms = %q, want none", cfg.ModerationTerms)
	}
}

func TestLoad_ToolRoundAndWikiDefaults(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxToolRounds != 2 || cfg.WikiEnabled || cfg.WikiBaseURL != WikiProductionURL {
		t.Errorf("got rounds=%d wiki=%v url=%q, want 2 false %q", cfg.MaxToolRounds, cfg.WikiEnabled, cfg.WikiBaseURL, WikiProductionURL)
	}
}

func TestLoad_MaxToolRoundsOutOfRangeFails(t *testing.T) {
	for _, v := range []string{"0", "7", "-1", "two"} {
		clearEnv(t)
		setRequired(t)
		t.Setenv("MAX_TOOL_ROUNDS", v)
		if _, err := Load(); err == nil {
			t.Errorf("MAX_TOOL_ROUNDS=%s loaded, want an error", v)
		}
	}
}

func TestLoad_TotalTimeoutMustCoverEveryRound(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("MAX_TOOL_ROUNDS", "4")
	t.Setenv("LLM_TIMEOUT_MS", "8000")
	t.Setenv("LLM_TOTAL_TIMEOUT_MS", "30000") // needs 40000
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LLM_TOTAL_TIMEOUT_MS") || !strings.Contains(err.Error(), "40000") {
		t.Fatalf("err = %v, want one naming LLM_TOTAL_TIMEOUT_MS and the 40000 it needs", err)
	}
	t.Setenv("LLM_TOTAL_TIMEOUT_MS", "45000")
	if _, err := Load(); err != nil {
		t.Fatalf("a covering budget still failed: %v", err)
	}
}

func TestLoad_WikiBaseURLIsPinned(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("WIKI_BASE_URL", "http://127.0.0.1:9999/api.php")
	if _, err := Load(); err == nil {
		t.Fatal("a non-production wiki URL loaded without WIKI_ALLOW_TEST_BASE_URL")
	}
	t.Setenv("WIKI_ALLOW_TEST_BASE_URL", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WikiBaseURL != "http://127.0.0.1:9999/api.php" {
		t.Errorf("WikiBaseURL = %q", cfg.WikiBaseURL)
	}
}

func TestLoad_WikiEnabledParsesBooleans(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "1": true, "false": false, "": false} {
		clearEnv(t)
		setRequired(t)
		t.Setenv("WIKI_ENABLED", v)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("WIKI_ENABLED=%q: %v", v, err)
		}
		if cfg.WikiEnabled != want {
			t.Errorf("WIKI_ENABLED=%q gave %v, want %v", v, cfg.WikiEnabled, want)
		}
	}
	clearEnv(t)
	setRequired(t)
	t.Setenv("WIKI_ENABLED", "yes please")
	if _, err := Load(); err == nil {
		t.Error("WIKI_ENABLED=\"yes please\" loaded, want an error")
	}
}
