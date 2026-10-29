package config

import "testing"

// The proxy URL keeps its scheme: socks5 vs socks5h selects local vs remote
// name resolution in clear.Transport, so collapsing the scheme into http would
// silently change behaviour.
func TestNormalizeProxySchemes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"127.0.0.1:2203", "http://127.0.0.1:2203"},
		{"http://127.0.0.1:2203", "http://127.0.0.1:2203"},
		{"https://proxy.example:8443", "https://proxy.example:8443"},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"socks5h://127.0.0.1:1080", "socks5h://127.0.0.1:1080"},
		{"SOCKS5://user:pass@10.0.0.2:1080", "socks5://user:pass@10.0.0.2:1080"},
		{"socks5://127.0.0.1:1080/", "socks5://127.0.0.1:1080/"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeProxy(tc.in)
			if err != nil {
				t.Fatalf("normalizeProxy(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeProxy(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeProxyRejects(t *testing.T) {
	cases := map[string]string{
		"socks4://127.0.0.1:1080":   "scheme must be",
		"ftp://127.0.0.1:21":        "scheme must be",
		"socks5://":                 "no host",
		"socks5://127.0.0.1:1080/x": "must not carry a path",
		"http://h:8080/path":        "must not carry a path",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := normalizeProxy(in)
			if err == nil {
				t.Fatalf("normalizeProxy(%q) accepted", in)
			}
			if !contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
		})
	}
}

// A socks proxy set through IL2API_PROXY must survive FromEnv unchanged, since
// the whole downstream behaviour keys on the scheme.
func TestFromEnvKeepsSOCKSProxy(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		if k == "IL2API_PROXY" {
			return "socks5://127.0.0.1:1080"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("Proxy = %q, want the socks5 URL unchanged", c.Proxy)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The conventional variables are a silent fallback: when IL2API_PROXY is unset
// but the shell still carries ALL_PROXY, the gateway uses it. Record where it
// came from so the boot log explains a proxy the operator believes they
// removed.
func TestProxySourceIsRecorded(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string // ProxySource
		up   string // resulting Proxy
	}{
		{"explicit", map[string]string{"IL2API_PROXY": "socks5://127.0.0.1:1080"}, "IL2API_PROXY", "socks5://127.0.0.1:1080"},
		{"inherit all_proxy", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080"}, "ALL_PROXY", "socks5://127.0.0.1:1080"},
		{"explicit wins over inherited", map[string]string{"IL2API_PROXY": "http://a:1", "ALL_PROXY": "socks5://b:2"}, "IL2API_PROXY", "http://a:1"},
		{"https_proxy lower", map[string]string{"https_proxy": "http://c:3"}, "https_proxy", "http://c:3"},
		{"none", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := FromEnv(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if c.ProxySource != tc.want {
				t.Fatalf("ProxySource = %q, want %q", c.ProxySource, tc.want)
			}
			if c.Proxy != tc.up {
				t.Fatalf("Proxy = %q, want %q", c.Proxy, tc.up)
			}
		})
	}
}
