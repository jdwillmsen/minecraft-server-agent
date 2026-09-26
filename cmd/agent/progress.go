package main

import (
	"context"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// progressText is whispered, never broadcast: only the asker is waiting, and
// a line in open chat for every slow answer would be noise to everyone else.
const progressText = "Looking that up…"

// progressDelay is how long a question can go unanswered before its asker is
// told it is coming. Most answers land well inside it, and for those the
// message would only be clutter.
const progressDelay = 3 * time.Second

// newProgressHook returns a tool-round hook that calls send at most once,
// on the first round finishing after delay has passed. Called on the
// answering goroutine, so no locking.
func newProgressHook(started time.Time, delay time.Duration, now func() time.Time, send func()) func(round int) {
	done := false
	return func(int) {
		if done || now().Sub(started) < delay {
			return
		}
		done = true
		send()
	}
}

// sendProgress returns the send newProgressHook calls: a whisper of
// progressText to the asker. A question from the server console gets none,
// because the console has no chat to whisper into.
func sendProgress(ctx context.Context, actorXUID string, pctx *plugin.Context, ans answering, log *logging.Logger) func() {
	return func() {
		if actorXUID == chat.ServerOrigin || pctx.Voice == nil {
			return
		}
		// Off the answering goroutine: the next model call should not wait
		// on the bridge, and the answer is still at least one model call
		// away, so it cannot overtake this.
		go func() {
			tellCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ans.broadcast)
			defer cancel()
			if err := pctx.Voice.Tell(tellCtx, actorXUID, progressText); err != nil {
				log.Error("mention_progress_send_failed", logging.Fields{"actor": actorXUID, "error": err.Error()})
			}
		}()
	}
}
