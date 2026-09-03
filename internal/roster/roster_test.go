package roster

import "testing"

func TestApply_NewPlayerReportedAsJoin(t *testing.T) {
	r := New()
	joins := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})

	if len(joins) != 1 {
		t.Fatalf("got %d joins, want 1", len(joins))
	}
	if joins[0] != (Entry{XUID: "111", Username: "Steve"}) {
		t.Errorf("join = %+v, want {111 Steve}", joins[0])
	}
}

func TestApply_KnownPlayerNotReportedAgain(t *testing.T) {
	r := New()
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})

	joins := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 0 {
		t.Errorf("got %d joins on a repeated add, want 0", len(joins))
	}
}

func TestApply_RemoveThenReAddReportsJoinAgain(t *testing.T) {
	r := New()
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	joins := r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	if len(joins) != 1 {
		t.Errorf("got %d joins after leave+rejoin, want 1", len(joins))
	}
}

func TestApply_BlankXUIDIgnored(t *testing.T) {
	r := New()
	joins := r.Apply([]PlayerListEntry{{XUID: "", Username: "Nobody"}})

	if len(joins) != 0 {
		t.Errorf("got %d joins for a blank XUID, want 0", len(joins))
	}
	if _, ok := r.NameFor(""); ok {
		t.Error("NameFor(\"\") = ok, want not-ok — a blank XUID must never resolve")
	}
}

func TestApply_MultipleNewPlayersInOneUpdate(t *testing.T) {
	r := New()
	joins := r.Apply([]PlayerListEntry{
		{XUID: "111", Username: "Steve"},
		{XUID: "222", Username: "Alex"},
	})

	if len(joins) != 2 {
		t.Fatalf("got %d joins, want 2", len(joins))
	}
}

func TestNameFor_UnknownXUID(t *testing.T) {
	r := New()
	if _, ok := r.NameFor("does-not-exist"); ok {
		t.Error("NameFor on an unknown XUID = ok, want not-ok")
	}
}

func TestNameFor_KnownXUIDAfterRemove(t *testing.T) {
	r := New()
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "Steve"}})
	r.Apply([]PlayerListEntry{{XUID: "111", Remove: true}})

	if _, ok := r.NameFor("111"); ok {
		t.Error("NameFor after removal = ok, want not-ok")
	}
}

func TestApply_UsernameUpdatesOnReAdd(t *testing.T) {
	r := New()
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "OldName"}})
	r.Apply([]PlayerListEntry{{XUID: "111", Username: "NewName"}})

	name, ok := r.NameFor("111")
	if !ok || name != "NewName" {
		t.Errorf("NameFor(111) = (%q, %v), want (NewName, true)", name, ok)
	}
}
