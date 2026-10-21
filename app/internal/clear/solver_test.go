package clear

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// Captured from a real Chromium session on 2026-10-21 against
// chat.inceptionlabs.ai. The browser's own solution must validate offline.
const (
	fixtureToken = "2.1792561477.60.YTBjYWY1NGIzZjI2ZGYwYjY4N2ViZWEzNTUwNWRkNzU7MGQ4NzY5ZjY7MjRkYjg1NzVmOGIxZWM2NjY0ODVhMGYxMTE1OGEwOTFhMGM1MmQzYjszO8qMVxGiXnqTXQHHX0yzC+VwwDPreUW+bVGhg/Zz/CBd33L+7PpkLBpFh3nanI4=.29eb5a2d9034b4909068b6ee1e7002ce"
	fixtureSol   = "5538cab0b44e3880;aa67964a55829353;c834280130a9bf5e"
)

func fixtureKey(seed int64) int {
	return int((((seed * int64(multipliers[((seed%5)+5)%5])) % 36) + 36) % 36)
}

// verify reproduces the server-side check: for each round, sha256(prefix+h)
// must start with the expected 4 hex chars, and the expected value for the next
// round is digest[offset:offset+4].
func verify(t *testing.T, token, solution string) bool {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	fields := strings.Split(string(raw), ";")
	seed := int64(0)
	for _, c := range parts[1] {
		seed = seed*10 + int64(c-'0')
	}
	cur := fields[2][fixtureKey(seed) : fixtureKey(seed)+prefixLen]
	for i, h := range strings.Split(solution, ";") {
		sum := sha256.Sum256([]byte(fields[1] + h))
		d := hex.EncodeToString(sum[:])
		if d[:prefixLen] != cur {
			return false
		}
		off := int((((seed * int64(multipliers[i%5])) % 60) + 60) % 60)
		cur = d[off : off+prefixLen]
	}
	return true
}

func TestSolverReproducesBrowserFixture(t *testing.T) {
	t.Parallel()
	// Arrange + Act: our solver is deterministic (counter nonces), so assert
	// the *property* it must satisfy rather than byte equality with Chromium.
	got, err := solveToken(fixtureToken)
	if err != nil {
		t.Fatalf("solveToken: %v", err)
	}
	// Assert
	if !verify(t, fixtureToken, got) {
		t.Fatalf("solver output does not satisfy PoW: %s", got)
	}
	if len(strings.Split(got, ";")) != 3 {
		t.Fatalf("want 3 rounds, got %q", got)
	}
}

func TestBrowserFixtureIsSelfConsistent(t *testing.T) {
	t.Parallel()
	// Guards the test helper itself against the captured real-world pair.
	if !verify(t, fixtureToken, fixtureSol) {
		t.Fatal("captured Chromium solution failed verification")
	}
}

func TestSolveTokenRejectsMalformed(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"no dots":        "garbage",
		"bad seed":       "2.abc.60.YWJj.ff",
		"bad base64":     "2.1.60.!!!.ff",
		"too few fields": "2.1.60." + base64.StdEncoding.EncodeToString([]byte("a;b")) + ".ff",
		"zero rounds":    "2.1.60." + base64.StdEncoding.EncodeToString([]byte("a;b;c;0;d")) + ".ff",
		"bad rounds":     "2.1.60." + base64.StdEncoding.EncodeToString([]byte("a;b;c;x;d")) + ".ff",
	}
	for name, tok := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := solveToken(tok); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestSolveTokenDifferentSeedShiftsKey(t *testing.T) {
	t.Parallel()
	// seed mod 5 = 0 -> multiplier 498787; verify offset arithmetic stays in range.
	for seed := int64(0); seed < 25; seed++ {
		k := fixtureKey(seed)
		if k < 0 || k >= 36 {
			t.Fatalf("seed %d produced out-of-range key %d", seed, k)
		}
	}
}

func TestSolveTokenTooShortTarget(t *testing.T) {
	t.Parallel()
	// key offset 0 with a 3-char target cannot supply a 4-char window.
	payload := base64.StdEncoding.EncodeToString([]byte("id;prefix;abc;1;bin"))
	if _, err := solveToken("2.0.60." + payload + ".sig"); err == nil {
		t.Fatal("expected an error for a too-short target")
	}
}
