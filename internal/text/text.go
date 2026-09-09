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
