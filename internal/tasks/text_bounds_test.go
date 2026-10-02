package tasks

import "testing"

func TestFenceFor(t *testing.T) {
	for _, tc := range []struct {
		text string
		min  int
		want string
	}{
		{"plain", 3, "```"},
		{"a ```` b", 3, "`````"},
		{"`x` and ``y``", 1, "```"},
		{"none", 1, "`"},
	} {
		if got := FenceFor(tc.text, tc.min); got != tc.want {
			t.Errorf("FenceFor(%q, %d) = %q, want %q", tc.text, tc.min, got, tc.want)
		}
	}
}

func TestCodeSpan(t *testing.T) {
	for text, want := range map[string]string{
		"user:alice": "`user:alice`",
		"a `b` c":    "``a `b` c``",
		"`edge":      "`` `edge ``",
		"":           "` `",
	} {
		if got := CodeSpan(text); got != want {
			t.Errorf("CodeSpan(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestCutBytesAndCutRunes(t *testing.T) {
	if got, cut := CutBytes("aжb", 2); got != "a" || !cut {
		t.Fatalf("CutBytes inside a rune = %q, %v", got, cut)
	}
	if got, cut := CutBytes("aжb", 4); got != "aжb" || cut {
		t.Fatalf("CutBytes at the length = %q, %v", got, cut)
	}
	if got, cut := CutRunes("жжж", 2); got != "жж" || !cut {
		t.Fatalf("CutRunes = %q, %v", got, cut)
	}
	if got, cut := CutRunes("жж", 2); got != "жж" || cut {
		t.Fatalf("CutRunes at the limit = %q, %v", got, cut)
	}
}
