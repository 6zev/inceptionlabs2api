package config

import "testing"

func TestMemoryLimitFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		set  bool
		want int64
	}{
		{"unset uses default", "", false, defaultMemoryLimit},
		{"empty uses default", "", true, defaultMemoryLimit},
		{"bytes", "268435456", true, 256 << 20},
		{"megabytes", "128M", true, 128 << 20},
		{"lowercase m", "512m", true, 512 << 20},
		{"gigabytes", "2G", true, 2 << 30},
		{"kilobytes", "1024K", true, 1024 << 10},
		{"zero disables", "0", true, 0},
		{"surrounding space", " 64M ", true, 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(string) (string, bool) { return tc.raw, tc.set }
			got, err := memoryLimitFromEnv(getenv)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMemoryLimitFromEnvRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"lots", "256MB", "-5", "M"} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			getenv := func(string) (string, bool) { return raw, true }
			if _, err := memoryLimitFromEnv(getenv); err == nil {
				t.Fatalf("%q accepted; want an error", raw)
			}
		})
	}
}
