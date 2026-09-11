package chat

import (
	"reflect"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

func TestIsPrivateTypeIsOnlyTheWhisper(t *testing.T) {
	if !IsPrivateType(packet.TextTypeWhisper) {
		t.Error("a /tell arrives as TextTypeWhisper and must count as private")
	}
	for _, tt := range []byte{packet.TextTypeChat, packet.TextTypeAnnouncement, packet.TextTypeRaw, packet.TextTypeSystem} {
		if IsPrivateType(tt) {
			t.Errorf("text type %d is not private and must not be treated as such", tt)
		}
	}
}

// Every private type has to be answerable too, or the agent would be
// classifying as private a message it never looks at in the first place.
func TestEveryPrivateTypeIsAlsoAnswerable(t *testing.T) {
	for tt := byte(0); tt < 32; tt++ {
		if IsPrivateType(tt) && !IsAnswerableType(tt) {
			t.Errorf("text type %d is private but not answerable", tt)
		}
	}
}

func TestIsAnswerableType(t *testing.T) {
	cases := map[byte]bool{
		packet.TextTypeChat:         true,
		packet.TextTypeWhisper:      true,
		packet.TextTypeAnnouncement: true,
		packet.TextTypeTranslation:  false,
		packet.TextTypePopup:        false,
		packet.TextTypeJukeboxPopup: false,
		packet.TextTypeTip:          false,
		packet.TextTypeSystem:       false,
		packet.TextTypeRaw:          false,
	}
	for textType, want := range cases {
		if got := IsAnswerableType(textType); got != want {
			t.Errorf("IsAnswerableType(%d) = %v, want %v", textType, got, want)
		}
	}
}

func TestIdentity_RealPlayer(t *testing.T) {
	pk := &packet.Text{SourceName: "Steve", XUID: "2535400000000000"}
	id, ok := Identity(pk)
	if !ok {
		t.Fatal("expected ok=true for a real player")
	}
	if id != "2535400000000000" {
		t.Errorf("id = %q, want XUID", id)
	}
}

func TestIdentity_ServerConsole(t *testing.T) {
	pk := &packet.Text{SourceName: "", XUID: ""}
	id, ok := Identity(pk)
	if !ok {
		t.Fatal("expected ok=true for server console message")
	}
	if id != ServerOrigin {
		t.Errorf("id = %q, want ServerOrigin", id)
	}
}

func TestIdentity_NameOnlyIsUnidentifiable(t *testing.T) {
	// A blank XUID with a non-empty name must never resolve to ServerOrigin
	// or to a usable identity - trusting the name here would let a player
	// impersonate the console.
	pk := &packet.Text{SourceName: "Steve", XUID: ""}
	_, ok := Identity(pk)
	if ok {
		t.Fatal("expected ok=false when XUID is empty but SourceName is not")
	}
}

func TestIdentity_GamertagIsNeverUsedAsIdentity(t *testing.T) {
	// Two different players could share a spoofed gamertag; identity must
	// come from XUID alone.
	a := &packet.Text{SourceName: "Notch", XUID: "111"}
	b := &packet.Text{SourceName: "Notch", XUID: "222"}
	idA, _ := Identity(a)
	idB, _ := Identity(b)
	if idA == idB {
		t.Fatal("identical gamertags must not produce identical identities when XUIDs differ")
	}
}

func TestIsSelfOrSibling(t *testing.T) {
	siblings := map[string]struct{}{"222": {}, "333": {}}

	if !IsSelfOrSibling("111", "111", siblings) {
		t.Error("expected self XUID to be flagged")
	}
	if !IsSelfOrSibling("222", "111", siblings) {
		t.Error("expected sibling XUID to be flagged")
	}
	if IsSelfOrSibling("999", "111", siblings) {
		t.Error("expected unrelated XUID not to be flagged")
	}
	if IsSelfOrSibling(ServerOrigin, "111", siblings) {
		t.Error("server origin must not be treated as self/sibling")
	}
}

func TestParseTrigger_Command(t *testing.T) {
	got := ParseTrigger("!help")
	want := Trigger{Kind: TriggerCommand, Command: "help", Args: []string{}}
	if got.Kind != want.Kind || got.Command != want.Command || !reflect.DeepEqual(got.Args, want.Args) {
		t.Errorf("ParseTrigger(%q) = %+v, want %+v", "!help", got, want)
	}
}

func TestParseTrigger_CommandWithArgs(t *testing.T) {
	got := ParseTrigger("!wp add home 100 64 -200")
	if got.Kind != TriggerCommand {
		t.Fatalf("Kind = %v, want TriggerCommand", got.Kind)
	}
	if got.Command != "wp" {
		t.Errorf("Command = %q, want wp", got.Command)
	}
	wantArgs := []string{"add", "home", "100", "64", "-200"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", got.Args, wantArgs)
	}
}

func TestParseTrigger_CommandIsCaseInsensitive(t *testing.T) {
	got := ParseTrigger("!HELP")
	if got.Command != "help" {
		t.Errorf("Command = %q, want lowercased help", got.Command)
	}
}

func TestParseTrigger_BareBangIsNotACommand(t *testing.T) {
	got := ParseTrigger("!")
	if got.Kind != TriggerNone {
		t.Errorf("Kind = %v, want TriggerNone for bare '!'", got.Kind)
	}
}

func TestParseTrigger_Mention(t *testing.T) {
	got := ParseTrigger("@server how do I craft a beacon")
	if got.Kind != TriggerMention {
		t.Fatalf("Kind = %v, want TriggerMention", got.Kind)
	}
	if got.Message != "@server how do I craft a beacon" {
		t.Errorf("Message = %q, want original text", got.Message)
	}
}

func TestParseTrigger_MentionIsCaseInsensitive(t *testing.T) {
	got := ParseTrigger("Hey @SERVER what time is it")
	if got.Kind != TriggerMention {
		t.Errorf("Kind = %v, want TriggerMention", got.Kind)
	}
}

func TestParseTrigger_CommandTakesPriorityOverMention(t *testing.T) {
	got := ParseTrigger("!ask @server what time is it")
	if got.Kind != TriggerCommand {
		t.Errorf("Kind = %v, want TriggerCommand (command prefix wins)", got.Kind)
	}
	if got.Command != "ask" {
		t.Errorf("Command = %q, want ask", got.Command)
	}
}

func TestParseTrigger_OrdinaryChatIsIgnored(t *testing.T) {
	got := ParseTrigger("anyone want to trade diamonds?")
	if got.Kind != TriggerNone {
		t.Errorf("Kind = %v, want TriggerNone for ordinary chat", got.Kind)
	}
}

func TestParseTrigger_EmptyMessage(t *testing.T) {
	got := ParseTrigger("")
	if got.Kind != TriggerNone {
		t.Errorf("Kind = %v, want TriggerNone for empty message", got.Kind)
	}
}

func TestParseTrigger_WhitespaceOnlyMessage(t *testing.T) {
	got := ParseTrigger("   \t  ")
	if got.Kind != TriggerNone {
		t.Errorf("Kind = %v, want TriggerNone for whitespace-only message", got.Kind)
	}
}

func TestParseTrigger_MentionSubstringInsideAnotherWord(t *testing.T) {
	// Deliberately permissive: the plan calls for a simple substring match,
	// not word-boundary detection, so this documents the current behaviour
	// rather than asserting it should change.
	got := ParseTrigger("myserverclient @serverside is broken")
	if got.Kind != TriggerMention {
		t.Errorf("Kind = %v, want TriggerMention (substring match is intentionally permissive)", got.Kind)
	}
}

func TestMessageEvent_KindIsTheChatMessageKind(t *testing.T) {
	var ev MessageEvent
	if ev.Kind() != MessageKind {
		t.Errorf("Kind() = %q, want %q", ev.Kind(), MessageKind)
	}
}
