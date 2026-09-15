package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// consoleBridge stands in for mc-console-bridge, which is a separate service
// from the Bedrock connection and is exactly what makes this test's premise
// true: it keeps answering while the agent is reconnecting.
type consoleBridge struct {
	mu       sync.Mutex
	commands []string
}

func (b *consoleBridge) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bridge received a body it could not decode: %v", err)
		}
		b.mu.Lock()
		b.commands = append(b.commands, body.Command)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rule":"tellraw"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (b *consoleBridge) ran() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.commands...)
}

// TestALateAnswerIsStillWhisperedAfterTheConnectionDies is the other half of
// the connection gap. handleMention answers on the process context
// deliberately -- the reply goes out over the console bridge, which is a
// separate service, so a reconnect mid-answer does not invalidate it. That
// only holds while the agent can still turn the asker's XUID into a
// gamertag: Voice.Tell carries nothing else, and a tellraw needs a name.
//
// This test fails if Roster.EndSession clears names along with presence. The
// answer then comes back to a roster that can no longer address the player
// who asked for it, Tell refuses, and the reply is logged as a failure and
// thrown away -- for the whole reconnect backoff, not the moment it takes a
// new session to receive its snapshot.
func TestALateAnswerIsStillWhisperedAfterTheConnectionDies(t *testing.T) {
	bridge := &consoleBridge{}
	srv := bridge.start(t)

	log := logging.New("info")
	playerRoster := roster.New()
	joins := newJoinTimes()
	// Wired exactly as main wires it: the roster is the voice's only name
	// resolver.
	voice := adapters.NewBridgeVoice(adapters.NewBridgeClient(srv.URL, "tok", time.Second), playerRoster)

	playerRoster.BeginSession(time.Now(), selfXUID)
	joins.connected()
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "ServerAgent"),
		addEntry(playerXUID, "LightKing0221"),
	), selfXUID, siblingBotXUIDs(), log, bus.New(), playerRoster, store.Nop{}, joins)

	// The player asks, and the model is still thinking when the connection
	// dies under it.
	connectionEnded(playerRoster, joins)

	if online := playerRoster.Online(); len(online) != 0 {
		t.Fatalf("Online() = %v in the gap, want nobody -- an announcement published now would be recorded against them", online)
	}
	if playerRoster.IsOnline(playerXUID) {
		t.Fatal("the asker still counts as present in the gap")
	}

	answer := "the nether hub is under spawn, straight down the stairs"
	if err := voice.Tell(context.Background(), playerXUID, answer); err != nil {
		t.Fatalf("Tell in the gap: %v -- the answer this player asked for is thrown away", err)
	}

	ran := bridge.ran()
	if len(ran) != 1 {
		t.Fatalf("bridge commands = %v, want exactly one tellraw", ran)
	}
	if !strings.HasPrefix(ran[0], `tellraw @a[name="LightKing0221"] `) {
		t.Errorf("command = %q, want it targeted at the player who asked", ran[0])
	}
	if !strings.Contains(ran[0], answer) {
		t.Errorf("command = %q, want it to carry the answer", ran[0])
	}
}
