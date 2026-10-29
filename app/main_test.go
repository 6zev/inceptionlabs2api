package main

import (
	"os"
	"testing"
)

// argsAndEnv sets the process args and an IL2API_UPDATE value for one
// subtest, restoring both afterwards.
func argsAndEnv(t *testing.T, args []string, env string) {
	t.Helper()
	oldArgs := os.Args
	oldEnv, hadEnv := os.LookupEnv("IL2API_UPDATE")
	os.Args = args
	if env == "" {
		os.Unsetenv("IL2API_UPDATE")
	} else {
		os.Setenv("IL2API_UPDATE", env)
	}
	t.Cleanup(func() {
		os.Args = oldArgs
		if hadEnv {
			os.Setenv("IL2API_UPDATE", oldEnv)
		} else {
			os.Unsetenv("IL2API_UPDATE")
		}
	})
}

func TestUpdateMode(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"long flag", []string{"inceptionlabs2api", "--update"}, "", true},
		{"short flag alias", []string{"inceptionlabs2api", "-update"}, "", true},
		{"subcommand", []string{"inceptionlabs2api", "update"}, "", true},
		{"subcommand after other args", []string{"inceptionlabs2api", "--verbose", "update"}, "", true},
		{"env on", []string{"inceptionlabs2api"}, "1", true},
		{"plain serve", []string{"inceptionlabs2api"}, "", false},
		// A misspelled subcommand must not start a gateway that looks healthy
		// while the operator believes it updated.
		{"typo", []string{"inceptionlabs2api", "updte"}, "", true},
		// The exec'd child: -serve must win over a stale env var, or the new
		// image would re-enter update mode and loop forever.
		{"serve beats env", []string{"inceptionlabs2api", "-serve"}, "1", false},
		{"serve beats nothing", []string{"inceptionlabs2api", "--serve"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argsAndEnv(t, tc.args, tc.env)
			if got := updateMode(); got != tc.want {
				t.Fatalf("updateMode() = %v, want %v", got, tc.want)
			}
		})
	}
}
