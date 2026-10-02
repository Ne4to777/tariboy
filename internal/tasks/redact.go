package tasks

import (
	"sort"
	"strings"
)

// Redaction of secret values from text the daemon returns or records. A value
// shorter than minRedactedSecret is left alone: it would blank ordinary words.
const (
	minRedactedSecret = 6
	redactedMarker    = "[redacted]"
)

// RedactSecrets replaces every occurrence of every value of at least 6 bytes
// in text with "[redacted]". All matches are found on the original text and
// merged first, and each merged stretch becomes one marker, so overlapping or
// adjacent values leave no partial value behind.
func RedactSecrets(text string, values []string) string {
	return redactSecretsFrom(text, values, 0)
}

// redactSecretsFrom is RedactSecrets for text[start:], with the matches found
// on the whole text: a value that begins before start and ends after it
// becomes a marker, and nothing before start is returned.
func redactSecretsFrom(text string, values []string, start int) string {
	var b strings.Builder
	pos := start
	for _, match := range secretMatches(text, values) {
		if match[1] <= pos {
			continue
		}
		if match[0] > pos {
			b.WriteString(text[pos:match[0]])
		}
		b.WriteString(redactedMarker)
		pos = match[1]
	}
	b.WriteString(text[pos:])
	return b.String()
}

// secretMatches returns the byte ranges of every occurrence of every value of
// at least minRedactedSecret bytes in text, sorted and merged: ranges that
// overlap or touch are one range.
func secretMatches(text string, values []string) [][2]int {
	var matches [][2]int
	for _, value := range values {
		if len(value) < minRedactedSecret {
			continue
		}
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], value)
			if i < 0 {
				break
			}
			matches = append(matches, [2]int{offset + i, offset + i + len(value)})
			offset += i + 1
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i][0] < matches[j][0] })
	merged := matches[:0]
	for _, match := range matches {
		if n := len(merged); n > 0 && match[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], match[1])
			continue
		}
		merged = append(merged, match)
	}
	return merged
}

// secretValues lists the values of a secret map.
func secretValues(secrets map[string]string) []string {
	values := make([]string, 0, len(secrets))
	for _, value := range secrets {
		values = append(values, value)
	}
	return values
}
