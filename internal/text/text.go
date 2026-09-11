// Package text holds the string handling the chat surfaces share.
package text

import "unicode/utf8"

// Truncate cuts s to at most limit bytes without splitting a rune.
//
// Gamertags, knowledge-base bodies and player-typed questions are all
// free-form UTF-8, so slicing at the cap can land inside a multi-byte rune
// and hand an invalid tail to whatever reads the result -- a model's prompt
// in one direction, Bedrock chat in the other. Every cap in this repo cuts
// here, so all of them cut the same way.
func Truncate(s string, limit int) string {
	if limit >= len(s) {
		return s
	}
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// ellipsis marks a cut for a human reader. One rune rather than three dots
// so it costs three bytes of the caller's budget instead of three
// characters of a chat line.
const ellipsis = "…"

// TruncateEllipsis cuts s like Truncate, but says so: a shortened result
// ends in an ellipsis, and still fits inside limit bytes.
//
// Separate from Truncate because only some caps are read by a person. A
// reply crossing Bedrock chat that stops mid-word reads as a finished
// thought and turns a formatting limit into an apparently confident wrong
// answer, so that one has to show its seam; a prompt or tool result trimmed
// on the way to a model has no reader to warn and should not spend bytes
// saying so.
//
// Under the ellipsis's own three bytes there is no room to mark anything,
// and limit is a hard bound rather than a target -- so the marker is what
// gives way, not the limit.
func TruncateEllipsis(s string, limit int) string {
	if limit >= len(s) {
		return s
	}
	if limit < len(ellipsis) {
		return Truncate(s, limit)
	}
	return Truncate(s, limit-len(ellipsis)) + ellipsis
}
