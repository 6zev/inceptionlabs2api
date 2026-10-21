package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// envMap turns a map into a getenv function.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Addr != ":3000" {
		t.Errorf("Addr = %q, want :3000", cfg.Addr)
	}
	if cfg.Model != "mercury-3" {
		t.Errorf("Model = %q, want mercury-3", cfg.Model)
	}
	if !reflect.DeepEqual(cfg.Models, []string{"mercury-3"}) {
		t.Errorf("Models = %v, want [mercury-3]", cfg.Models)
	}
	if cfg.PoolMin != 2 || cfg.PoolMax != 8 {
		t.Errorf("pool = %d..%d, want 2..8", cfg.PoolMin, cfg.PoolMax)
	}
	if cfg.PoolTTL != 30*time.Minute {
		t.Errorf("PoolTTL = %s, want 30m", cfg.PoolTTL)
	}
	if cfg.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %s, want 60s", cfg.IdleTimeout)
	}
	if cfg.ContextChars != 250_000 {
		t.Errorf("ContextChars = %d, want 250000", cfg.ContextChars)
	}
	if cfg.Proxy != "" {
		t.Errorf("Proxy = %q, want empty", cfg.Proxy)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(map[string]string{
		"IL2API_ADDR":            "127.0.0.1:8080",
		"IL2API_API_KEY":         "secret",
		"IL2API_MODEL":           "mercury-2",
		"IL2API_MODELS":          " mercury-2 , mercury-3 ,, ",
		"IL2API_POOL_MIN":        "1",
		"IL2API_POOL_MAX":        "4",
		"IL2API_POOL_TTL":        "10m",
		"IL2API_IDLE_TIMEOUT":    "5s",
		"IL2API_CONTEXT_CHARS":   "-1",
		"IL2API_SESSION_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" || cfg.APIKey != "secret" || cfg.Model != "mercury-2" {
		t.Errorf("scalar overrides not applied: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Models, []string{"mercury-2", "mercury-3"}) {
		t.Errorf("Models = %v, want [mercury-2 mercury-3]", cfg.Models)
	}
	if cfg.ContextChars != -1 {
		t.Errorf("ContextChars = %d, want -1 (disabled)", cfg.ContextChars)
	}
	if cfg.PoolTTL != 10*time.Minute || cfg.IdleTimeout != 5*time.Second || cfg.SessionTimeout != 2*time.Second {
		t.Errorf("durations not applied: %+v", cfg)
	}
}

func TestProxyResolution(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"explicit bare host", map[string]string{"IL2API_PROXY": "127.0.0.1:18080"}, "http://127.0.0.1:18080"},
		{"explicit url", map[string]string{"IL2API_PROXY": "http://proxy:3128"}, "http://proxy:3128"},
		{"https proxy env", map[string]string{"HTTPS_PROXY": "http://egress:9000"}, "http://egress:9000"},
		{"http proxy env", map[string]string{"HTTP_PROXY": "http://egress:9001"}, "http://egress:9001"},
		{"all proxy env", map[string]string{"ALL_PROXY": "egress:9002"}, "http://egress:9002"},
		{"explicit wins", map[string]string{"IL2API_PROXY": "http://a:1", "HTTPS_PROXY": "http://b:2"}, "http://a:1"},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := FromEnv(envMap(tc.env))
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.Proxy != tc.want {
				t.Errorf("Proxy = %q, want %q", cfg.Proxy, tc.want)
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"min above max", map[string]string{"IL2API_POOL_MIN": "9", "IL2API_POOL_MAX": "8"}, "must not exceed"},
		{"negative max", map[string]string{"IL2API_POOL_MAX": "0"}, "POOL_MAX"},
		{"zero context", map[string]string{"IL2API_CONTEXT_CHARS": "0"}, "CONTEXT_CHARS"},
		{"negative context", map[string]string{"IL2API_CONTEXT_CHARS": "-2"}, "CONTEXT_CHARS"},
		{"bad int", map[string]string{"IL2API_POOL_MIN": "two"}, "must be an integer"},
		{"bad duration", map[string]string{"IL2API_POOL_TTL": "30"}, "must be a duration"},
		{"zero duration", map[string]string{"IL2API_IDLE_TIMEOUT": "0s"}, "must be positive"},
		{"bad upstream", map[string]string{"IL2API_UPSTREAM_BASE": "chat.inceptionlabs.ai"}, "http(s) URL"},
		{"bad addr", map[string]string{"IL2API_ADDR": "3000"}, "host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnv(envMap(tc.env))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRedactProxy(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"http://user:secret@proxy:3128": "http://proxy:3128",
		"http://proxy:3128":             "http://proxy:3128",
		"http://user@proxy:3128":        "http://proxy:3128",
	}
	for in, want := range cases {
		if got := RedactProxy(in); got != want {
			t.Errorf("RedactProxy(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedactProxy("://bad"); got != "<invalid proxy URL>" {
		t.Errorf("RedactProxy(invalid) = %q", got)
	}
}

func TestProxyCredentialsDoNotAppearInErrors(t *testing.T) {
	t.Parallel()
	_, err := FromEnv(envMap(map[string]string{"IL2API_PROXY": "http://user:supersecret@proxy:3128"}))
	if err != nil {
		t.Fatalf("valid proxy rejected: %v", err)
	}
	_, err = FromEnv(envMap(map[string]string{"IL2API_PROXY": "ftp://user:supersecret@proxy:3128"}))
	if err == nil {
		t.Fatal("expected scheme error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error leaks proxy password: %v", err)
	}
}
