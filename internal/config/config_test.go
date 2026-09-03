package config

import (
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MC_HOST", "MC_USERNAME", "MC_PORT",
		"RECONNECT_MIN_MS", "RECONNECT_MAX_MS", "HTTP_ADDR", "AUTH_CACHE_DIR",
		"COMMAND_RATE_LIMIT_PER_MINUTE", "LOG_LEVEL",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoad_MissingHostFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_USERNAME", "agent-bot")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when MC_HOST is unset")
	}
}

func TestLoad_MissingUsernameFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when MC_USERNAME is unset")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")

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
}

func TestLoad_PortOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
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
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("MC_PORT", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-integer MC_PORT")
	}
}

func TestLoad_ZeroPortFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("MC_PORT", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for zero MC_PORT")
	}
}

func TestLoad_PortAboveValidRangeFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("MC_PORT", "99999999")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for MC_PORT above 65535")
	}
}

func TestLoad_MaxValidPortSucceeds(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("MC_PORT", "65535")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCPort != 65535 {
		t.Errorf("MCPort = %d, want 65535", cfg.MCPort)
	}
}

func TestLoad_ReconnectMaxBelowMinFails(t *testing.T) {
	clearEnv(t)
	t.Setenv("MC_HOST", "mc.example.internal")
	t.Setenv("MC_USERNAME", "agent-bot")
	t.Setenv("RECONNECT_MIN_MS", "10000")
	t.Setenv("RECONNECT_MAX_MS", "1000")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when RECONNECT_MAX_MS < RECONNECT_MIN_MS")
	}
}

func TestLoad_HostAndUsernameAreTrimmed(t *testing.T) {
	clearEnv(t)
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
