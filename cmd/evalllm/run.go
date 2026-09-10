package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/toolset"
)

// Observation is everything one case produced, before any judgement.
type Observation struct {
	// Reply is what the agent would have sent: collapsed and cut to the
	// chat limit by the production client.
	Reply string
	Err   error
	// Private is whether production would whisper the reply rather than
	// broadcast it.
	Private bool
	// Latency covers the whole answer, tool rounds included, as the asker
	// experiences it.
	Latency time.Duration
	Rounds  []Round
}

// ToolsCalled is every tool the model asked for, in order, including names
// the toolset never offered.
func (o Observation) ToolsCalled() []string {
	var names []string
	for _, r := range o.Rounds {
		for _, c := range r.ToolCalls {
			names = append(names, c.Name)
		}
	}
	return names
}

// finalRound is the exchange whose text became the reply.
func (o Observation) finalRound() (Round, bool) {
	for i := len(o.Rounds) - 1; i >= 0; i-- {
		if o.Rounds[i].Status == http.StatusOK {
			return o.Rounds[i], true
		}
	}
	return Round{}, false
}

// runner puts one case at a time to the model through the production
// answer path.
type runner struct {
	client   *adapters.LLMClient
	recorder *Recorder
	// total bounds one whole answer, as LLM_TOTAL_TIMEOUT_MS does in
	// production: a model that overruns it is heard by nobody.
	total time.Duration
	world func() *plugin.Context
}

func (r runner) run(ctx context.Context, c Case) Observation {
	// Built fresh per case, as production builds one per answer: the
	// whisper flag it returns belongs to this one question.
	registry, scoped := toolset.Build(r.world())
	r.recorder.Reset()

	answerCtx, cancel := context.WithTimeout(ctx, r.total)
	defer cancel()
	start := time.Now()
	reply, err := r.client.AnswerWithTools(answerCtx, c.Asker, c.XUID, c.Question, registry)
	latency := time.Since(start)

	return Observation{
		Reply: reply,
		Err:   err,
		// The rule handleMention applies before choosing a whisper over a
		// broadcast.
		Private: scoped.Happened() && c.XUID != chat.ServerOrigin,
		Latency: latency,
		Rounds:  r.recorder.Rounds(),
	}
}
