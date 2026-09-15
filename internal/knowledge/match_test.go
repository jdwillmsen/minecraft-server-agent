package knowledge

import "testing"

var goldFarm = Entry{
	Topic: "gold farm",
	Body:  "The gold farm is in the nether at 120 64 -340, reached through the portal at spawn.",
}

var tradingHall = Entry{
	Topic: "hall",
	Body:  "The villager trading hall is east of spawn at 250 65 10. The mending librarian is in stall 4.",
}

func TestMarkPartialMatches(t *testing.T) {
	cases := []struct {
		name  string
		query string
		entry Entry
		want  MatchKind
	}{
		{
			// The bug: the query and the topic are both farms, and that is
			// all they are. The entry says nothing about slime, so it
			// answers a different question from the one asked.
			name:  "query names another kind of the topic's head",
			query: "where is the slime farm",
			entry: goldFarm,
			want:  MatchPartial,
		},
		{
			name:  "query names the topic's own modifier",
			query: "wheres the gold farm",
			entry: goldFarm,
			want:  MatchExact,
		},
		{
			// "the farm" asks for whichever farm there is, and there is
			// one. Nothing contradicts it, so nothing to hedge.
			name:  "query names the head with no modifier of its own",
			query: "where is the farm",
			entry: goldFarm,
			want:  MatchExact,
		},
		{
			// "nether" and "spawn" are both words of this body, and neither
			// says which farm: the body places the gold farm in the nether
			// and routes there through spawn. Reading either as the entry
			// agreeing it is that farm answers a question about another one
			// with the gold farm's coordinates, stated as fact.
			name:  "query modifies the head with a word the body only mentions in passing",
			query: "where is the spawn farm",
			entry: goldFarm,
			want:  MatchPartial,
		},
		{
			name:  "modifier appears in the body in front of the head",
			query: "wheres the villager trading hall",
			entry: tradingHall,
			want:  MatchExact,
		},
		{
			// "mending" is in this body, describing a librarian rather than
			// a hall, so it is no evidence the entry is about the hall the
			// question asks for.
			name:  "topic filed as the head, asked about as another compound",
			query: "where is the mending hall",
			entry: tradingHall,
			want:  MatchPartial,
		},
		{
			// The production shape: !kb set takes one whitespace-delimited
			// argument as the topic, so no operator can file a fact under
			// "gold farm" -- the compound only ever reaches the store
			// inside the body. A rule reading compounds out of the topic
			// alone leaves this, the reported failure, unhedged.
			name:  "one-word topic whose body names the compound",
			query: "where is the slime farm",
			entry: Entry{Topic: "gold", Body: "The gold farm is in the nether at 120 64 -340."},
			want:  MatchPartial,
		},
		{
			name:  "one-word topic asked about by its own compound",
			query: "where is the gold farm",
			entry: Entry{Topic: "gold", Body: "The gold farm is in the nether at 120 64 -340."},
			want:  MatchExact,
		},
		{
			// The query names the topic's own modifier, just not up against
			// the head. Hedging this denies a hit the entry answers exactly.
			name:  "another adjective between the topic's modifier and the head",
			query: "where is the iron golem farm",
			entry: Entry{Topic: "iron", Body: "The iron farm is east of spawn at 200 64 50."},
			want:  MatchExact,
		},
		{
			// Most of this question is absent from the entry that answers
			// it, and the entry is still the right answer: a rule counting
			// uncovered query words would hedge this one.
			name:  "uncovered words that modify nothing in the topic",
			query: "where can i get mending books",
			entry: Entry{Topic: "trading hall", Body: "The villager trading hall is east of spawn at 250 65 10. The mending librarian is in stall 4."},
			want:  MatchExact,
		},
		{
			// A one-word topic has no modifier of its own for the query to
			// contradict, so "server restart" is a way of asking about
			// "restarts" rather than a different thing that restarts.
			name:  "one-word topic modified by the query",
			query: "when does the server restart each day",
			entry: Entry{Topic: "restarts", Body: "Restarts happen every day at 09:00 UTC and take about two minutes."},
			want:  MatchExact,
		},
		{
			name:  "one-word topic with a trailing filler word",
			query: "what are the rules here",
			entry: Entry{Topic: "rules", Body: "No griefing, no stealing from chests, no lag machines."},
			want:  MatchExact,
		},
		{
			name:  "query does not use the topic's head at all",
			query: "where is the slime farm",
			entry: Entry{Topic: "end portal", Body: "The stronghold with the end portal is at -1200 30 800 in the overworld."},
			want:  MatchExact,
		},
		{
			name:  "plural of the head still reads as the same head",
			query: "where are the slime farms",
			entry: goldFarm,
			want:  MatchPartial,
		},
		{
			// A fallback-only row is already the weaker claim of the two;
			// re-reading it as a partial would upgrade a guess.
			name:  "a fallback-only row keeps its own kind",
			query: "where is the slime farm",
			entry: Entry{Topic: "gold farm", Body: "under spawn", Matched: MatchFallback},
			want:  MatchFallback,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := []Entry{tc.entry}
			MarkPartialMatches(tc.query, entries)
			if entries[0].Matched != tc.want {
				t.Errorf("Matched = %v, want %v", entries[0].Matched, tc.want)
			}
		})
	}
}

// The eval fixtures score the same words production does, so a case cannot
// pass on a word one side drops and the other keeps.
func TestSignificantWord(t *testing.T) {
	cases := map[string]bool{
		"gold": true, "slime": true, "tnt": true, "end": true,
		"the": false, "here": false, "about": false, "have": false, "is": false, "x": false,
	}
	for word, want := range cases {
		if got := SignificantWord(word); got != want {
			t.Errorf("SignificantWord(%q) = %v, want %v", word, got, want)
		}
	}
}

func TestSameStem(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"farm", "farm", true},
		{"farms", "farm", true},
		{"restart", "restarts", true},
		{"farm", "gold", false},
		// Too short to take a prefix match on: "end" is a topic word in its
		// own right and must not read as "ender" or "ending".
		{"end", "ender", false},
	}
	for _, tc := range cases {
		if got := SameStem(tc.a, tc.b); got != tc.want {
			t.Errorf("SameStem(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
