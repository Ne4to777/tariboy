package tasks

import "testing"

func TestRedactSecrets(t *testing.T) {
	for name, tc := range map[string]struct {
		text   string
		values []string
		want   string
	}{
		"one value":           {"token=s3cr3t-value done", []string{"s3cr3t-value"}, "token=[redacted] done"},
		"every occurrence":    {"a s3cr3t-value b s3cr3t-value", []string{"s3cr3t-value"}, "a [redacted] b [redacted]"},
		"short values stay":   {"abc s3cr3t-value", []string{"abc", "s3cr3t-value"}, "abc [redacted]"},
		"contained value":     {"x s3cr3t-value y s3cr3t z", []string{"s3cr3t", "s3cr3t-value"}, "x [redacted] y [redacted] z"},
		"overlapping values":  {"xxabcdefghijklyy", []string{"abcdefgh", "efghijkl"}, "xx[redacted]yy"},
		"adjacent values":     {"<first1second>", []string{"first1", "second"}, "<[redacted]>"},
		"overlapping repeats": {"[aaaaaaaa]", []string{"aaaaaa"}, "[[redacted]]"},
		"no values":           {"plain", nil, "plain"},
		"empty value":         {"plain", []string{""}, "plain"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RedactSecrets(tc.text, tc.values); got != tc.want {
				t.Fatalf("RedactSecrets(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestRedactSecretsFrom(t *testing.T) {
	values := []string{"s3cr3t-value"}
	// Text before start is never returned; a value that straddles start
	// becomes one marker.
	if got := redactSecretsFrom("lead s3cr3t-value tail", values, 8); got != "[redacted] tail" {
		t.Fatalf("straddling = %q", got)
	}
	if got := redactSecretsFrom("lead s3cr3t-value tail", values, 5); got != "[redacted] tail" {
		t.Fatalf("at start = %q", got)
	}
	if got := redactSecretsFrom("lead-text s3cr3t-value", values, 4); got != "-text [redacted]" {
		t.Fatalf("before = %q", got)
	}
}
