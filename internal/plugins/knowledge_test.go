package plugins

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

type fakeKnowledge struct {
	entries  map[string]knowledge.Entry
	upserted int
}

func newFakeKnowledge() *fakeKnowledge {
	return &fakeKnowledge{entries: map[string]knowledge.Entry{}}
}

func (f *fakeKnowledge) Lookup(_ context.Context, q string, limit int) ([]knowledge.Entry, error) {
	var out []knowledge.Entry
	for _, e := range f.entries {
		if strings.Contains(e.Topic, knowledge.NormalizeTopic(q)) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeKnowledge) Upsert(_ context.Context, topic, body, author string) error {
	f.upserted++
	f.entries[knowledge.NormalizeTopic(topic)] = knowledge.Entry{
		Topic: knowledge.NormalizeTopic(topic), Body: body, AuthorXUID: author, UpdatedAt: time.Now(),
	}
	return nil
}
func (f *fakeKnowledge) Delete(_ context.Context, topic string) (bool, error) {
	key := knowledge.NormalizeTopic(topic)
	if _, ok := f.entries[key]; !ok {
		return false, nil
	}
	delete(f.entries, key)
	return true, nil
}
func (f *fakeKnowledge) List(context.Context) ([]knowledge.Entry, error) {
	out := make([]knowledge.Entry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeKnowledge) Enabled() bool { return true }

func kbCommand(t *testing.T) plugin.Command {
	t.Helper()
	for _, c := range NewKnowledge().Commands() {
		if c.Name == "kb" {
			return c
		}
	}
	t.Fatal("kb command not registered")
	return plugin.Command{}
}

func TestKBWriteRequiresOperator(t *testing.T) {
	cmd := kbCommand(t)
	if cmd.Permission != plugin.PermissionVisitor {
		t.Fatalf("kb base permission = %v, want visitor (reads are open)", cmd.Permission)
	}

	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "member",
		ActorPermission: plugin.PermissionMember,
		Args:            []string{"set", "rules", "be", "nice"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.upserted != 0 {
		t.Fatal("a member wrote to the knowledge store")
	}
	if !strings.Contains(strings.ToLower(reply), "operator") {
		t.Errorf("reply %q should say why the write was refused", reply)
	}
}

func TestKBSetThenGet(t *testing.T) {
	cmd := kbCommand(t)
	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"set", "gold", "farm", "is", "under", "spawn"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "visitor",
		ActorPermission: plugin.PermissionVisitor,
		Args:            []string{"gold"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(reply, "under spawn") {
		t.Errorf("reply = %q, want the stored body", reply)
	}
}

func TestKBWithoutStore(t *testing.T) {
	cmd := kbCommand(t)
	reply, err := cmd.Run(context.Background(), &plugin.Context{}, plugin.Invocation{
		ActorXUID: "someone", ActorPermission: plugin.PermissionVisitor, Args: []string{"rules"},
	})
	if err != nil {
		t.Fatalf("a nil Knowledge must not error: %v", err)
	}
	if reply == "" {
		t.Error("reply should explain the feature is unconfigured")
	}
}

func TestKBSetRefusesReservedTopic(t *testing.T) {
	cmd := kbCommand(t)
	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"set", "list", "not", "actually", "a", "subcommand"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.upserted != 0 {
		t.Fatal("a reserved topic name was stored")
	}
	if !strings.Contains(strings.ToLower(reply), "reserved") {
		t.Errorf("reply %q should say the name is reserved", reply)
	}
}

// A confirmed delete of a topic that was never there teaches an operator
// that the fact is gone, so the one that is still being quoted at players
// goes unexamined. The waypoint store already reports this honestly.
func TestKBDeleteReportsWhenNothingExisted(t *testing.T) {
	cmd := kbCommand(t)
	pctx := &plugin.Context{Knowledge: newFakeKnowledge()}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"del", "nope"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "forgotten") {
		t.Errorf("reply %q claims a delete that never happened", reply)
	}
}

func TestKBDeleteConfirmsARealDelete(t *testing.T) {
	cmd := kbCommand(t)
	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}
	op := plugin.Invocation{ActorXUID: "op", ActorPermission: plugin.PermissionOperator}

	set := op
	set.Args = []string{"set", "rules", "be", "nice"}
	if _, err := cmd.Run(context.Background(), pctx, set); err != nil {
		t.Fatalf("set: %v", err)
	}

	del := op
	del.Args = []string{"del", "Rules"}
	reply, err := cmd.Run(context.Background(), pctx, del)
	if err != nil {
		t.Fatalf("del: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "forgotten") {
		t.Errorf("reply = %q, want it to confirm the delete", reply)
	}
	if len(fake.entries) != 0 {
		t.Errorf("entries = %v, want the topic gone", fake.entries)
	}
}
