package config

import (
	"os"
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
	if cfg.ContextChars != 0 {
		t.Errorf("ContextChars = %d, want the deprecated no-op default 0", cfg.ContextChars)
	}
	if cfg.Proxy != "" {
		t.Errorf("Proxy = %q, want empty", cfg.Proxy)
	}
	if cfg.ClearanceRefresh != 45*time.Minute {
		t.Errorf("ClearanceRefresh = %s, want 45m (refresh inside the 60m cookie TTL)", cfg.ClearanceRefresh)
	}
	if cfg.ClearanceCooldown != 15*time.Second {
		t.Errorf("ClearanceCooldown = %s, want 15s", cfg.ClearanceCooldown)
	}
	if cfg.ClearancePool != 3 {
		t.Errorf("ClearancePool = %d, want the default 3", cfg.ClearancePool)
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
		{"negative context", map[string]string{"IL2API_CONTEXT_CHARS": "-2"}, "CONTEXT_CHARS"},
		{"zero clearance refresh", map[string]string{"IL2API_CLEARANCE_REFRESH": "0s"}, "CLEARANCE_REFRESH"},
		{"negative clearance cooldown", map[string]string{"IL2API_CLEARANCE_COOLDOWN": "-1s"}, "CLEARANCE_COOLDOWN"},
		{"zero clearance pool", map[string]string{"IL2API_CLEARANCE_POOL": "0"}, "CLEARANCE_POOL"},
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

// The deprecated knob is accepted for deployment compatibility but must not
// be described anywhere as doing something. The deployment files are part of
// the contract: shipping a non-zero default in docker-compose would tell an
// operator a 250 000-character guard exists (SPEC §1.1 says it does not).
func TestDeploymentFilesDoNotSetTheDeprecatedContextKnob(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"../../../docker-compose.yml", "../../.env.example"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, "IL2API_CONTEXT_CHARS") && !strings.Contains(trimmed, ":-0}") {
				t.Errorf("%s:%d sets IL2API_CONTEXT_CHARS to a non-zero value: %s", path, i+1, trimmed)
			}
		}
	}
}

func TestSearchDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.SearchTimeout != 60*time.Second {
		t.Errorf("SearchTimeout = %s, want 60s", cfg.SearchTimeout)
	}
	if !cfg.SearchConfigCheck {
		t.Error("SearchConfigCheck = false, want true by default")
	}
	if cfg.SearchProbeTimeout != 15*time.Second {
		t.Errorf("SearchProbeTimeout = %s, want 15s", cfg.SearchProbeTimeout)
	}
}

func TestClearanceOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(map[string]string{
		"IL2API_CLEARANCE_REFRESH":  "30m",
		"IL2API_CLEARANCE_COOLDOWN": "5s",
		"IL2API_CLEARANCE_POOL":     "5",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClearanceRefresh != 30*time.Minute || cfg.ClearanceCooldown != 5*time.Second {
		t.Fatalf("clearance durations not applied: %+v", cfg)
	}
	if cfg.ClearancePool != 5 {
		t.Fatalf("ClearancePool = %d, want 5", cfg.ClearancePool)
	}
}

func TestSearchOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(map[string]string{
		"IL2API_SEARCH_TIMEOUT":       "30s",
		"IL2API_SEARCH_PROBE_TIMEOUT": "5s",
		"IL2API_SEARCH_CONFIG_CHECK":  "OFF",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.SearchTimeout != 30*time.Second || cfg.SearchProbeTimeout != 5*time.Second {
		t.Errorf("search durations not applied: %+v", cfg)
	}
	if cfg.SearchConfigCheck {
		t.Error("SearchConfigCheck = true, want false for OFF")
	}
}

func TestSearchConfigValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad toggle", map[string]string{"IL2API_SEARCH_CONFIG_CHECK": "maybe"}, "on or off"},
		{"zero search timeout", map[string]string{"IL2API_SEARCH_TIMEOUT": "0s"}, "SEARCH_TIMEOUT"},
		{"zero probe timeout", map[string]string{"IL2API_SEARCH_PROBE_TIMEOUT": "-1s"}, "SEARCH_PROBE_TIMEOUT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnv(envMap(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestAudioDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.AudioTimeout != 120*time.Second {
		t.Errorf("AudioTimeout = %s, want 120s", cfg.AudioTimeout)
	}
	if cfg.AudioMaxBytes != 25<<20 {
		t.Errorf("AudioMaxBytes = %d, want 26214400", cfg.AudioMaxBytes)
	}
}

func TestAudioOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := FromEnv(envMap(map[string]string{
		"IL2API_AUDIO_TIMEOUT":   "45s",
		"IL2API_AUDIO_MAX_BYTES": "1048576",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.AudioTimeout != 45*time.Second || cfg.AudioMaxBytes != 1<<20 {
		t.Fatalf("audio settings not applied: %+v", cfg)
	}
}

func TestAudioConfigValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"zero timeout", map[string]string{"IL2API_AUDIO_TIMEOUT": "0s"}, "AUDIO_TIMEOUT"},
		{"zero bytes", map[string]string{"IL2API_AUDIO_MAX_BYTES": "0"}, "AUDIO_MAX_BYTES"},
		{"non-numeric bytes", map[string]string{"IL2API_AUDIO_MAX_BYTES": "big"}, "AUDIO_MAX_BYTES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := FromEnv(envMap(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
