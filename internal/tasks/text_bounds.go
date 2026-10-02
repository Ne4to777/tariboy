package tasks

import (
	"strings"
	"unicode/utf8"
)

// Bounds and quoting for untrusted text the daemon shows an agent or the
// customer: the Goal block and the pause comment share them.

// CutMarker ends a value that was cut to its bound.
const CutMarker = "… (cut)"

// FenceFor returns a run of backticks longer than any backtick run in text and
// at least minLen long: a code fence (minLen 3) or a code span delimiter
// (minLen 1) that nothing inside text can close.
func FenceFor(text string, minLen int) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(minLen, longest+1))
}

// CodeSpan renders one line of untrusted text as a Markdown code span. Text
// that starts or ends with a backtick is padded with a space on both sides,
// which Markdown strips again; empty text becomes a span of one space.
func CodeSpan(text string) string {
	fence := FenceFor(text, 1)
	switch {
	case text == "":
		text = " "
	case strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`"):
		text = " " + text + " "
	}
	return fence + text + fence
}

// CutBytes cuts s to at most n bytes on a rune boundary and reports whether
// it cut anything.
func CutBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// CutRunes cuts s to at most n runes and reports whether it cut anything.
func CutRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i], true
		}
		count++
	}
	return s, false
}
