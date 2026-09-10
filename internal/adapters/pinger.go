package adapters

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const (
	gametimeCommand = "time query gametime"
	// tpsMinWindow is the shortest span a TPS figure is measured over. The
	// game clock is read in whole ticks and dated to the millisecond, so over
	// a span of a few seconds that rounding alone moves the answer visibly.
	tpsMinWindow = 10 * time.Second
	// tpsMaxWindow is the longest. The game clock stops while the server is
	// down, so a baseline from before a restart reports the downtime as lag;
	// capping the span keeps that error to the minute or two after one.
	tpsMaxWindow = 2 * time.Minute
	// maxTickSamples bounds the history. The window only ever needs the
	// newest few readings.
	maxTickSamples = 32
	// pingHeadroom is held back from the caller's deadline for the reply.
	// A bridge call allowed to run to the same deadline as the command loses
	// that race, and the player hears a timeout instead of the half of the
	// answer that was already measured.
	pingHeadroom = time.Second
	// maxLoggedOutput bounds the console text copied into a parse-failure
	// log line; enough to see what the server said instead.
	maxLoggedOutput = 200
)

// errNoGametime is the console answering, but not with a game time. Kept
// apart from a transport failure because it points at a different cause --
// the server's output changed shape -- and says so at a louder log level.
var errNoGametime = errors.New("pinger: the console answered without a Gametime line")

// errNoAnswer is the console saying nothing inside the bridge's collect
// window, which is not the same fault as errNoGametime. A server lagging by
// more than that window prints its Gametime line a moment after the bridge
// has stopped listening -- seen in production at 18 TPS -- so this is
// transient, and it is the laggy moments that go unmeasured. Treating it as a
// parser regression would raise the loudest alarm for the most ordinary cause.
var errNoAnswer = errors.New("pinger: the console printed nothing within the bridge's collect window")

// gametimeLine matches Bedrock's answer to `time query gametime`. The log
// prefix, when present, carries the server's own clock to the millisecond.
// That dates a reading exactly, where the agent's clock would also count
// however long the request queued behind other commands at the bridge.
var gametimeLine = regexp.MustCompile(`(?:\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}):(\d{3})[^\]]*\]\s*)?Gametime is (\d+)`)

type tickSample struct {
	ticks int64
	at    time.Time
	// serverClock marks at as the server's log stamp rather than the agent's
	// clock. Readings from the two are never compared with each other.
	serverClock bool
}

// ServerPinger measures TPS from the server's game clock through the console
// bridge, and link latency from the agent's live Bedrock connection.
//
// Timing the bridge call itself would measure nothing: the bridge collects
// console output for a fixed window before it answers, so every call takes
// about as long as every other.
type ServerPinger struct {
	client *BridgeClient
	link   func() (time.Duration, bool)
	log    *logging.Logger
	now    func() time.Time

	mu      sync.Mutex
	samples []tickSample
}

// NewServerPinger builds a ServerPinger. link reports the current Bedrock
// round trip, or false when there is no session; it may be nil.
func NewServerPinger(client *BridgeClient, link func() (time.Duration, bool), log *logging.Logger) *ServerPinger {
	return &ServerPinger{client: client, link: link, log: log, now: time.Now}
}

var _ plugin.Pinger = (*ServerPinger)(nil)

// Sample reads the game clock once and keeps the reading as a baseline. TPS
// is a rate, so a !ping with no older reading to compare against can only
// say it is still measuring; running this periodically is what saves every
// ping from that.
func (p *ServerPinger) Sample(ctx context.Context) error {
	_, _, err := p.sample(ctx)
	return err
}

// Ping takes a fresh reading and reports TPS against the most recent
// baseline in the window, alongside the current link latency.
func (p *ServerPinger) Ping(ctx context.Context) plugin.ServerPing {
	var out plugin.ServerPing
	if p.link != nil {
		out.Link, out.LinkKnown = p.link()
	}

	bridgeCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		bridgeCtx, cancel = context.WithDeadline(ctx, deadline.Add(-pingHeadroom))
		defer cancel()
	}
	out.TPS, out.TPSKnown, out.TPSErr = p.sample(bridgeCtx)
	if out.TPSErr != nil && !errors.Is(out.TPSErr, errNoGametime) {
		// A player asked and was told TPS is unavailable; the reason belongs
		// in the log. A missing Gametime line has already been logged, louder,
		// by sample.
		p.log.Warn("ping_console_failed", logging.Fields{"error": out.TPSErr.Error()})
	}
	return out
}

func (p *ServerPinger) sample(ctx context.Context) (tps float64, known bool, err error) {
	sent := p.now()
	resp, err := p.client.runCommand(ctx, gametimeCommand)
	if err != nil {
		return 0, false, fmt.Errorf("pinger: %w", err)
	}
	if strings.TrimSpace(resp.Output) == "" {
		// Not logged here: a player's !ping reports it at Warn, and the
		// background sampler at Debug, which is the right weight for a
		// server that answered a second late.
		return 0, false, errNoAnswer
	}
	cur, err := parseGametime(resp.Output, sent)
	if err != nil {
		// Error rather than Warn: the console is up and answering, so this is
		// the server's output no longer matching what the pinger expects, and
		// every !ping and background reading fails the same way until someone
		// changes the parser.
		p.log.Error("tps_gametime_unparsed", logging.Fields{"error": err.Error(), "output": text.Truncate(resp.Output, maxLoggedOutput)})
		return 0, false, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	tps, known = tpsAgainst(p.samples, cur)
	p.samples = append(p.samples, cur)
	if len(p.samples) > maxTickSamples {
		p.samples = p.samples[len(p.samples)-maxTickSamples:]
	}
	// Recorded here rather than by either caller so a player's !ping and the
	// background sampler both refresh the gauge: whichever measured last is
	// the freshest value there is. Stamped with the agent's clock at the
	// request, not the server's log stamp: an alert compares it with the
	// scraper's time(), which the server's zone-less stamp cannot be.
	if known {
		metrics.ServerTPS(tps, sent)
	}
	return tps, known, nil
}

// parseGametime reads the last Gametime line in output. The bridge runs one
// command at a time, but its collect window can still catch console lines
// from elsewhere, and the newest match is the one this command produced.
func parseGametime(output string, sent time.Time) (tickSample, error) {
	all := gametimeLine.FindAllStringSubmatch(output, -1)
	if len(all) == 0 {
		return tickSample{}, errNoGametime
	}
	m := all[len(all)-1]
	ticks, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return tickSample{}, fmt.Errorf("%w: gametime %q: %v", errNoGametime, m[3], err)
	}
	if m[1] != "" {
		// Parsed as UTC whatever zone the server logs in: only differences
		// between two stamps are ever used. A DST change lands a difference
		// outside the window, which drops that pair rather than misreading it.
		if at, err := time.Parse(time.DateTime, m[1]); err == nil {
			ms, _ := strconv.Atoi(m[2])
			return tickSample{ticks: ticks, at: at.Add(time.Duration(ms) * time.Millisecond), serverClock: true}, nil
		}
	}
	return tickSample{ticks: ticks, at: sent}, nil
}

// tpsAgainst measures cur against the newest usable baseline in history.
// Newest rather than oldest because a TPS figure is a question about now:
// a longer span averages a lag spike away.
//
// A baseline with more ticks than cur means the world's clock went
// backwards -- a restored or replaced world -- and is never usable.
func tpsAgainst(history []tickSample, cur tickSample) (float64, bool) {
	var base tickSample
	found := false
	for _, s := range history {
		span := cur.at.Sub(s.at)
		if s.serverClock != cur.serverClock || span < tpsMinWindow || span > tpsMaxWindow || s.ticks > cur.ticks {
			continue
		}
		if !found || s.at.After(base.at) {
			base, found = s, true
		}
	}
	if !found {
		return 0, false
	}
	return float64(cur.ticks-base.ticks) / cur.at.Sub(base.at).Seconds(), true
}
