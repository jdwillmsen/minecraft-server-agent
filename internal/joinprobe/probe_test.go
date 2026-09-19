package joinprobe

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sandertv/go-raknet"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// The pong a real server sends, with the fields this probe reads in the
// positions production uses. Taken from the live server rather than invented:
// MCPE;<motd>;<protocol>;<version>;<online>;<max>;<guid>;...
const samplePong = "MCPE;FWB Server;2193;1.26.51;3;20;13467591190557326198;FWB;Survival;1;19132;19133;"

// fakeServer is a real RakNet listener that answers the handshake however a
// case asks it to. Real RakNet rather than a stubbed transport, because the
// framing this probe writes is the part most likely to be wrong, and a stub
// would agree with whatever the probe did.
type fakeServer struct {
	t     *testing.T
	l     *raknet.Listener
	reply func(clientProtocol int32) []packet.Packet
	// rawReply answers with bytes rather than packets, which is the only way
	// to stage a reply no well-formed encoder would produce.
	rawReply func() []byte
	closed   chan struct{}
}

func newFakeServer(t *testing.T, pong string, reply func(int32) []packet.Packet) *fakeServer {
	t.Helper()
	l, err := raknet.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.PongData([]byte(pong))

	s := &fakeServer{t: t, l: l, reply: reply, closed: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() {
		_ = l.Close()
		<-s.closed
	})
	return s
}

func (s *fakeServer) addr() string { return s.l.Addr().String() }

func (s *fakeServer) serve() {
	defer close(s.closed)
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	SetDeadline(time.Time) error
}) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	batch, err := packet.NewDecoder(conn).Decode()
	if err != nil || len(batch) == 0 {
		return
	}
	buf := bytes.NewBuffer(batch[0])
	header := &packet.Header{}
	if err := header.Read(buf); err != nil || header.PacketID != packet.IDRequestNetworkSettings {
		return
	}
	pk := &packet.RequestNetworkSettings{}
	pk.Marshal(protocol.NewReader(buf, 0, false))

	if s.rawReply != nil {
		_ = packet.NewEncoder(conn).Encode([][]byte{s.rawReply()})
		return
	}
	if s.reply == nil {
		return
	}
	var out [][]byte
	for _, reply := range s.reply(pk.ClientProtocol) {
		out = append(out, encode(reply))
	}
	if len(out) > 0 {
		_ = packet.NewEncoder(conn).Encode(out)
	}
}

func TestProbeReachesTheHandshakeOnAHealthyServer(t *testing.T) {
	s := newFakeServer(t, samplePong, func(int32) []packet.Packet {
		return []packet.Packet{&packet.NetworkSettings{CompressionThreshold: 512}}
	})

	got := Probe(context.Background(), s.addr(), 0)

	if !got.Joinable() {
		t.Fatalf("stage = %s, want handshake (err: %v)", got.Stage, got.Err)
	}
	// The probe has to read the protocol out of the pong to know what to
	// announce, and getting that wrong is how it would test the wrong thing
	// while still passing.
	if got.DialedProtocol != 2193 {
		t.Errorf("dialled protocol %d, want the advertised 2193", got.DialedProtocol)
	}
	if got.Version != "1.26.51" {
		t.Errorf("version %q, want the advertised 1.26.51", got.Version)
	}
}

// The 2026-09-15 outage in miniature: the server is a version behind the
// client and refuses before any credential is looked at. Every other check in
// this repo passed through that, which is the whole reason this probe exists.
func TestProbeReportsAServerThatRefusesTheProtocol(t *testing.T) {
	s := newFakeServer(t, samplePong, func(clientProtocol int32) []packet.Packet {
		if clientProtocol > 2169 {
			return []packet.Packet{&packet.PlayStatus{Status: packet.PlayStatusLoginFailedServer}}
		}
		return []packet.Packet{&packet.NetworkSettings{}}
	})

	got := Probe(context.Background(), s.addr(), 2193)

	if got.Stage != StageRefused {
		t.Fatalf("stage = %s, want refused (err: %v)", got.Stage, got.Err)
	}
	if got.PlayStatus != packet.PlayStatusLoginFailedServer {
		t.Errorf("play status %d, want LoginFailedServer (%d)", got.PlayStatus, packet.PlayStatusLoginFailedServer)
	}
	if got.Joinable() {
		t.Error("a refused session reported itself as joinable")
	}
}

// A forced protocol is what makes the probe able to ask "could a client built
// against *this* version join", rather than only "is the server consistent
// with itself".
func TestProbeHonoursAForcedProtocol(t *testing.T) {
	var seen int32
	s := newFakeServer(t, samplePong, func(clientProtocol int32) []packet.Packet {
		seen = clientProtocol
		return []packet.Packet{&packet.NetworkSettings{}}
	})

	Probe(context.Background(), s.addr(), 2200)

	if seen != 2200 {
		t.Errorf("server saw protocol %d, want the forced 2200", seen)
	}
}

func TestProbeReportsAnUnreachableServer(t *testing.T) {
	// A port nothing is listening on: RakNet has no answer to give, which is
	// the state a dead server or an unrouted NodePort produces.
	got := Probe(context.Background(), "127.0.0.1:1", 0)

	if got.Stage != StageUnreachable {
		t.Fatalf("stage = %s, want unreachable", got.Stage)
	}
	if got.Err == nil {
		t.Error("an unreachable server produced no error to report")
	}
}

// A server that answers the ping and then says nothing is the shape of one
// mid-restart, or one too busy to service a join. It must not be reported as
// joinable, and it must not hang the probe either.
func TestProbeReportsAServerThatGoesSilentAfterThePing(t *testing.T) {
	s := newFakeServer(t, samplePong, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := Probe(ctx, s.addr(), 0)

	if got.Stage != StagePong {
		t.Fatalf("stage = %s, want pong", got.Stage)
	}
	if got.Err == nil {
		t.Error("a silent handshake produced no error to report")
	}
}

// gophertunnel's protocol.Reader reports a short read by panicking, and
// nothing recovers it out here. A probe that dies on a malformed reply
// reports nothing at exactly the moment the server is behaving strangely,
// which is when it is most wanted.
func TestProbeSurvivesATruncatedPlayStatus(t *testing.T) {
	s := newFakeServer(t, samplePong, nil)
	s.rawReply = func() []byte {
		// A valid PlayStatus header with no body at all.
		buf := &bytes.Buffer{}
		header := packet.Header{PacketID: packet.IDPlayStatus}
		_ = header.Write(buf)
		return buf.Bytes()
	}

	got := Probe(context.Background(), s.addr(), 0)

	if got.Joinable() {
		t.Fatal("a truncated play status was read as a joinable server")
	}
	// Named rather than merely non-nil, so the case cannot pass because the
	// probe failed somewhere earlier and never read the reply at all.
	if got.Err == nil || !strings.Contains(got.Err.Error(), "play status body") {
		t.Errorf("error = %v, want one naming the short play status body", got.Err)
	}
}

func TestProbeReportsADisconnectAsNotJoinable(t *testing.T) {
	s := newFakeServer(t, samplePong, func(int32) []packet.Packet {
		return []packet.Packet{&packet.Disconnect{Message: "server full"}}
	})

	got := Probe(context.Background(), s.addr(), 0)

	if got.Joinable() {
		t.Fatal("a server that disconnected the probe was reported joinable")
	}
	if got.Stage != StagePong {
		t.Errorf("stage = %s, want pong for a disconnected handshake", got.Stage)
	}
}
