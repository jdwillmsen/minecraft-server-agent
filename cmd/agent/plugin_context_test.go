package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// testPluginContext builds the context exactly as main does with no database
// configured: every capability present, every one of them the disabled
// implementation. Anything main assembles for itself (the voice, the
// deliverer) is assembled the same way here, so a field this test proves is
// wired is a field production actually gets.
func testPluginContext() *plugin.Context {
	cfg := config.Config{}
	bridgeClient := adapters.NewBridgeClient("http://bridge.invalid", "token", time.Second)
	playerRoster := roster.New()
	voice := adapters.NewBridgeVoice(bridgeClient, playerRoster)
	deliverer := announce.NewDeliverer(
		announce.Nop{},
		voice,
		newDeliveryAudience(playerRoster, siblingBotXUIDs()),
		announcePermissions{resolver: adapters.NewPermissionResolver(bridgeClient, time.Second, logging.New("error"))},
		logging.New("error"),
	)
	return newPluginContext(
		cfg,
		bridgeClient,
		time.Second,
		voice,
		playerRoster,
		plugin.NewRegistry(),
		store.Nop{},
		knowledge.Nop{},
		waypoints.Nop{},
		announce.Nop{},
		deliverer,
	)
}

// Every field on plugin.Context is something a plugin may reach for. One left
// unassigned is invisible: the field exists, the code compiles, and the plugin
// that needed it takes the "not configured" branch forever.
//
// That is not hypothetical. Profiles was declared and never assigned, so the
// welcome plugin never called RecordJoin, no arrival was ever persisted, and
// nothing logged -- the only branch that would have reported it is the error
// path of a call that was not happening. Two tables sat empty in production
// while the agent reported itself healthy.
//
// Reflection rather than a field-by-field list on purpose: a new field added
// to plugin.Context and forgotten here is exactly the bug, so the test has to
// notice fields it was never told about.
func TestEveryPluginContextFieldIsWired(t *testing.T) {
	v := reflect.ValueOf(testPluginContext()).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if f := v.Field(i); f.IsZero() {
			t.Errorf("plugin.Context.%s is unset; plugins reading it will silently take their disabled branch", name)
		}
	}
}

// The specific regression, stated on its own so a failure names the cause
// rather than a field index.
func TestProfilesIsWiredToTheStore(t *testing.T) {
	if testPluginContext().Profiles == nil {
		t.Fatal("Profiles is nil: player joins would never be recorded, and nothing would say so")
	}
}

// store.Nop stands in when no database is configured. It must still be a
// usable value rather than a nil interface, because the caller cannot tell
// the difference until a plugin dereferences it.
func TestProfilesIsUsableWithoutADatabase(t *testing.T) {
	profiles := testPluginContext().Profiles
	if profiles.Enabled() {
		t.Error("a context built with store.Nop reports persistence as enabled")
	}
	if _, err := profiles.RecordJoin(t.Context(), "2535400000000000", "SomePlayer", time.Now()); err != nil {
		t.Errorf("recording a join against the no-op store failed: %v", err)
	}
}
