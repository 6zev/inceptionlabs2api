package clear

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The v2 proof-of-work solved by the Vercel Security Checkpoint page. Ported
// verbatim from the verified PoC (/root/vercel-bypass/poc/main.go); the
// algorithm is a 3-round SHA-256 partial-prefix chain.

// prefixLen is the number of hex characters matched per round (65 536 hashes
// on average per round).
const prefixLen = 4

// multipliers come from the wasm solver and pick the 4-hex-char slice of a
// digest that the next round must match.
var multipliers = [5]int{498787, 533737, 619763, 708403, 828071}

// solveToken implements the v2 PoW. Token layout:
//
//	<version>.<seed>.<ttl>.<b64(payload)>.<signature>
//
// payload = <id>;<prefix>;<target>;<rounds>;<binary>
//
// Each round finds a suffix h such that sha256(prefix+h) starts with the
// current 4-hex-char target; the next target is a 4-char slice of that digest
// at a seed-dependent offset. The nonce is a counter, not randomness: the
// server does not care what h is, only that the digests match.
func solveToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 4 {
		return "", fmt.Errorf("malformed token: %d dot-parts", len(parts))
	}
	seed, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("seed: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return "", fmt.Errorf("payload b64: %w", err)
	}
	fields := strings.Split(string(raw), ";")
	if len(fields) < 4 {
		return "", fmt.Errorf("payload fields: %d", len(fields))
	}
	prefix, target := fields[1], fields[2]
	rounds, err := strconv.Atoi(fields[3])
	if err != nil {
		return "", fmt.Errorf("rounds: %w", err)
	}
	if rounds < 1 {
		return "", fmt.Errorf("rounds: %d", rounds)
	}

	// key = seed*multiplier(seed mod 5) mod 36 -> start offset into target.
	mul := int64(multipliers[((seed%5)+5)%5])
	key := int((((seed * mul) % 36) + 36) % 36)
	if len(target) < key+prefixLen {
		return "", errors.New("target too short for key offset")
	}
	cur := target[key : key+prefixLen]

	out := make([]string, 0, rounds)
	var nonce uint64
	for i := 0; i < rounds; i++ {
		found := false
		for !found {
			h := fmt.Sprintf("%016x", nonce)
			nonce++
			sum := sha256.Sum256([]byte(prefix + h))
			digest := hex.EncodeToString(sum[:])
			if digest[:prefixLen] != cur {
				continue
			}
			out = append(out, h)
			off := int((((seed * int64(multipliers[i%5])) % 60) + 60) % 60)
			cur = digest[off : off+prefixLen]
			found = true
		}
	}
	return strings.Join(out, ";"), nil
}
