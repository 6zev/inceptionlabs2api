package upstream

import (
	"net/url"
	"strings"
)

// Text-shape rules shared by the two source lists: /v1/web_search results
// (§2.3.2) and chat url_citation annotations (§2.4). They live here so the
// two collectors cannot drift (SPEC §6.1); each caller supplies its own count
// cap and passes an already-trimmed URL.
//
// These are output-shape rules, not request validation: the gateway's
// documented contract says these fields are links and titles, so a
// javascript: URL or a title full of raw HTML whitespace would make that
// contract false in a consumer that renders links.
const (
	maxTitleRunes = 512
	maxURLRunes   = 4096
)

// isHTTPURL reports whether raw is an absolute http(s) URL with a host.
func isHTTPURL(raw string) bool {
	if len(raw) < len("http://")+1 {
		return false
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// hasControl reports whether s contains ASCII control characters, which
// could smuggle headers or terminal escapes into downstream consumers.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// usableSource reports whether url may be emitted as a link: absolute http(s),
// within the rune bound, and free of control characters.
func usableSource(url string) bool {
	return isHTTPURL(url) && len([]rune(url)) <= maxURLRunes && !hasControl(url)
}

// sourceTitle normalises an upstream title: whitespace collapsed, then
// bounded in runes.
func sourceTitle(title string) string {
	return truncateRunes(collapseSpace(title), maxTitleRunes)
}

// truncateRunes returns s cut to at most n runes, preserving valid UTF-8.
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
