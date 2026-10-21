package upstream

import (
	"strings"
	"testing"
)

func TestIsHTTPURL(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"https://example.com/a": true,
		"http://example.com":    true,
		"HTTPS://Example.COM/x": true, // the scheme check is case-insensitive
		"javascript:alert(1)":   false,
		"data:text/html,x":      false,
		"/relative":             false,
		"ftp://example.com":     false,
		"https://":              false,
		"https:///path":         false,
		"https://example.com":   true,
	}
	// Callers trim before asking; the rule is only "absolute http(s), with a
	// host".
	for in, want := range cases {
		if got := isHTTPURL(in); got != want {
			t.Errorf("isHTTPURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestHasControl(t *testing.T) {
	t.Parallel()
	if !hasControl("https://x/\x00") || !hasControl("a\nb") || !hasControl("a\x7fb") {
		t.Fatal("control characters should be detected")
	}
	if hasControl("https://x/路径") {
		t.Fatal("non-ASCII text is not a control character")
	}
}

// usableSource is the one acceptance rule both source lists share.
func TestUsableSource(t *testing.T) {
	t.Parallel()
	if !usableSource("https://ok.example/a") {
		t.Fatal("an absolute https URL should be usable")
	}
	rejected := []string{
		"javascript:alert(1)",
		"data:text/html,<script>",
		"/relative",
		"https://ok.example/\x00",
		"https://ok.example/" + strings.Repeat("a", maxURLRunes),
	}
	for _, u := range rejected {
		if usableSource(u) {
			t.Errorf("usableSource(%q) = true, want false", u)
		}
	}
}

func TestSourceTitle(t *testing.T) {
	t.Parallel()
	if got := sourceTitle("\n\tFour-day  work week\n \n – X"); got != "Four-day work week – X" {
		t.Fatalf("title = %q", got)
	}
	long := strings.Repeat("标", maxTitleRunes+10)
	if got := len([]rune(sourceTitle(long))); got != maxTitleRunes {
		t.Fatalf("title runes = %d, want %d", got, maxTitleRunes)
	}
}

func TestTruncateRunesPreservesUTF8(t *testing.T) {
	t.Parallel()
	s := "四天工作制"
	for n := 0; n <= len([]rune(s)); n++ {
		got := truncateRunes(s, n)
		if len([]rune(got)) != n {
			t.Fatalf("truncateRunes(%q, %d) = %q (%d runes)", s, n, got, len([]rune(got)))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("truncateRunes(%q, %d) is not a prefix", s, n)
		}
	}
}
