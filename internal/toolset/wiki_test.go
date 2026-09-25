package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/wiki"
)

type stubWiki struct {
	out           string
	err           error
	topic, aspect string
}

func (s *stubWiki) Lookup(_ context.Context, topic, aspect string) (string, error) {
	s.topic, s.aspect = topic, aspect
	return s.out, s.err
}

func (s *stubWiki) Enabled() bool { return true }

func TestWikiLookupIsAbsentWithoutAWiki(t *testing.T) {
	registry, _ := Build(&plugin.Context{})
	if registry.Has("wiki_lookup") {
		t.Error("wiki_lookup registered with no wiki configured")
	}
}

func TestWikiLookupIsAbsentWhenTheWikiIsDisabled(t *testing.T) {
	registry, _ := Build(&plugin.Context{Wiki: wiki.Nop{}})
	if registry.Has("wiki_lookup") {
		t.Error("wiki_lookup registered with a disabled wiki")
	}
}

func TestWikiLookupPassesTopicAndAspect(t *testing.T) {
	w := &stubWiki{out: strings.Repeat("r", 1100)}
	registry, scoped := Build(&plugin.Context{Wiki: w})
	out, err := registry.Invoke(t.Context(), "wiki_lookup", json.RawMessage(`{"topic":"torch","aspect":"crafting"}`), "2535400000000001")
	if err != nil {
		t.Fatal(err)
	}
	if w.topic != "torch" || w.aspect != "crafting" {
		t.Errorf("wiki asked for %q/%q", w.topic, w.aspect)
	}
	if len(out) < 1100 {
		t.Errorf("result cut to %d chars, want the wiki tool's own 1,200 cap", len(out))
	}
	if scoped.Happened() {
		t.Error("a wiki lookup marked the answer as personal")
	}
}

func TestWikiLookupFailuresReachTheModelAsFixedText(t *testing.T) {
	for err, want := range map[error]string{
		wiki.ErrNotFound: `no minecraft.wiki page found for "herobrine"`,
		fmt.Errorf("%w: dial 10.0.0.1", wiki.ErrUnavailable): "minecraft.wiki is unavailable right now",
		wiki.ErrLimited: "minecraft.wiki is unavailable right now",
	} {
		registry, _ := Build(&plugin.Context{Wiki: &stubWiki{err: err}})
		out, invokeErr := registry.Invoke(t.Context(), "wiki_lookup", json.RawMessage(`{"topic":"herobrine"}`), "")
		if invokeErr != nil {
			t.Fatalf("%v surfaced as an error, want a result the model can answer around", invokeErr)
		}
		if out != want {
			t.Errorf("%v -> %q, want %q", err, out, want)
		}
		if strings.Contains(out, "10.0.0.1") {
			t.Errorf("internal detail leaked to the model: %q", out)
		}
	}
}

func TestWikiLookupRejectsMalformedArguments(t *testing.T) {
	registry, _ := Build(&plugin.Context{Wiki: &stubWiki{}})
	if _, err := registry.Invoke(t.Context(), "wiki_lookup", json.RawMessage(`{"topic":`), ""); err == nil {
		t.Error("malformed arguments were accepted")
	}
}
