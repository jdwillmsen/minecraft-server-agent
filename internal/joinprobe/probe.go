// Package joinprobe answers a question none of this cluster's other checks
// ask: can a client still start a session with the server?
//
// Every check that existed during the 2026-09-15 outage passed throughout it.
// The RakNet server-list ping answered, mc-monitor reported the server online
// with players connected, and all three kubelet probes were green — while no
// retail client could join, because the server was a version behind and hard
// kicks a mismatched protocol before login begins. The two clients that were
// connected held sessions established before the outage and implement RakNet
// themselves, so their presence argued the opposite of the truth.
//
// This probe goes one step past the ping and stops one step short of an
// account. It performs the pre-login handshake — the exchange where a server
// either accepts a client's protocol or rejects it — and reports which
// happened. That is the exact step that failed on 2026-09-15, and it needs no
// Xbox Live identity, because the version verdict is delivered before any
// credential is examined.
//
// What it deliberately does not prove: that a real player can authenticate,
// spawn and play. Proving that needs a real account and is a separate,
// human-gated piece of work.
package joinprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sandertv/go-raknet"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcproto"
)

// Stage is how far a probe got. The values are ordered, so an alert can say
// "below Handshake" rather than enumerating failures, and a new stage can be
// added at the top without rewriting the rules.
type Stage int

const (
	// StageUnreachable means RakNet never answered. The server is down, the
	// node is down, or nothing is routing to the port.
	StageUnreachable Stage = iota
	// StagePong means the server answered the server-list ping. This is
	// where every check this repo had before this one stopped, and it is
	// exactly what stayed green through the 2026-09-15 outage.
	StagePong
	// StageRefused means the server answered the handshake by refusing the
	// session. A client cannot join a server in this state; the play status
	// says why.
	StageRefused
	// StageHandshake means the server accepted the protocol and replied with
	// its network settings, which is the last thing that happens before a
	// client sends credentials.
	StageHandshake
)

func (s Stage) String() string {
	switch s {
	case StagePong:
		return "pong"
	case StageRefused:
		return "refused"
	case StageHandshake:
		return "handshake"
	default:
		return "unreachable"
	}
}

// Result is one probe attempt.
type Result struct {
	Stage Stage
	// Protocol and Version are what the server advertised in its pong, which
	// is how the probe learns what to dial with when no protocol is forced.
	Protocol int32
	Version  string
	// DialedProtocol is the number the handshake announced.
	DialedProtocol int32
	// PlayStatus is the refusal the server sent, valid only at StageRefused.
	// packet.PlayStatusLoginFailedClient means the client is older than the
	// server; LoginFailedServer means the reverse, which is the shape of the
	// outage this probe exists for.
	PlayStatus int32
	Duration   time.Duration
	Err        error
}

// Joinable reports whether a session could be started. It is the one question
// callers should ask; the stages exist to say why not.
func (r Result) Joinable() bool { return r.Stage == StageHandshake }

const (
	// pingTimeout and handshakeTimeout are separate because they fail for
	// different reasons: a ping that times out means nothing is listening,
	// while a handshake that times out means something answered and then
	// stopped — a server mid-restart, or one too busy to service a join.
	pingTimeout      = 5 * time.Second
	handshakeTimeout = 10 * time.Second
)

// Probe runs one attempt against address.
//
// forceProtocol pins the protocol number the handshake announces. Zero means
// "whatever the server advertised", which tests the path a current client
// takes; a specific number tests whether a client built against that protocol
// would be let in, which is the question a version skew poses.
func Probe(ctx context.Context, address string, forceProtocol int32) Result {
	started := time.Now()
	result := Result{DialedProtocol: forceProtocol}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	pong, err := raknet.PingContext(pingCtx, address)
	cancel()
	if err != nil {
		result.Duration = time.Since(started)
		result.Err = fmt.Errorf("ping %s: %w", address, err)
		return result
	}
	result.Stage = StagePong

	// A pong that does not parse is not fatal on its own: the server answered,
	// and a forced protocol can still be tested against it. Without one there
	// is nothing to dial with, so that case stops here.
	ad, parseErr := mcproto.ParsePong(string(pong))
	if parseErr == nil {
		result.Protocol, result.Version = ad.Protocol, ad.Version
	}
	if result.DialedProtocol == 0 {
		if parseErr != nil {
			result.Duration = time.Since(started)
			result.Err = fmt.Errorf("read advertised protocol: %w", parseErr)
			return result
		}
		result.DialedProtocol = ad.Protocol
	}

	status, handshakeErr := handshake(ctx, address, result.DialedProtocol)
	result.Duration = time.Since(started)
	switch {
	case handshakeErr != nil:
		result.Err = handshakeErr
	case status != nil:
		result.Stage, result.PlayStatus = StageRefused, *status
	default:
		result.Stage = StageHandshake
	}
	return result
}

// handshake sends RequestNetworkSettings and reads the server's answer.
//
// It returns a non-nil status when the server refused the session, and nil,
// nil when it accepted it. Compression and encryption are deliberately left
// off: both are negotiated *by* the packet being sent here, so this exchange
// is plaintext for a real client too.
func handshake(ctx context.Context, address string, clientProtocol int32) (*int32, error) {
	dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	conn, err := raknet.DialContext(dialCtx, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	deadline, ok := dialCtx.Deadline()
	if ok {
		_ = conn.SetDeadline(deadline)
	}

	request := &packet.RequestNetworkSettings{ClientProtocol: clientProtocol}
	if err := packet.NewEncoder(conn).Encode([][]byte{encode(request)}); err != nil {
		return nil, fmt.Errorf("send request network settings: %w", err)
	}

	decoder := packet.NewDecoder(conn)
	// One batch can carry several packets, and a refusal may arrive beside a
	// disconnect. Reading a bounded number of batches rather than one keeps a
	// chatty server from being read as a silent one.
	for range 4 {
		batch, err := decoder.Decode()
		if err != nil {
			return nil, fmt.Errorf("read handshake reply: %w", err)
		}
		for _, raw := range batch {
			id, payload, err := split(raw)
			if err != nil {
				return nil, err
			}
			switch id {
			case packet.IDNetworkSettings:
				return nil, nil
			case packet.IDPlayStatus:
				pk := &packet.PlayStatus{}
				pk.Marshal(protocol.NewReader(bytes.NewReader(payload), 0, false))
				status := pk.Status
				return &status, nil
			case packet.IDDisconnect:
				// A disconnect without a play status is still a refusal, and
				// the server's own reason is in the packet; the caller gets
				// the stage, and the reason belongs in the error.
				return nil, errors.New("server disconnected during the handshake")
			}
		}
	}
	return nil, errors.New("server answered the handshake with neither network settings nor a play status")
}

// encode marshals a packet with its header, the way a client's encoder does.
func encode(pk packet.Packet) []byte {
	buf := &bytes.Buffer{}
	header := packet.Header{PacketID: pk.ID()}
	_ = header.Write(buf)
	pk.Marshal(protocol.NewWriter(buf, 0))
	return buf.Bytes()
}

// split takes the header off a packet and returns its ID and the rest.
func split(raw []byte) (uint32, []byte, error) {
	buf := bytes.NewBuffer(raw)
	header := &packet.Header{}
	if err := header.Read(buf); err != nil {
		return 0, nil, fmt.Errorf("read packet header: %w", err)
	}
	return header.PacketID, buf.Bytes(), nil
}
