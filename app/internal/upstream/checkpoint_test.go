package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The mock upstream verifies solutions with the same algorithm the checkpoint
// uses, so a regression in the solver fails here rather than at the live edge.

var testMultipliers = [5]int{498787, 533737, 619763, 708403, 828071}

const testPrefixLen = 4

// checkpointToken builds a syntactically valid v2 token. The target is all
// zeros so the solver's scan terminates quickly and deterministically.
func checkpointToken(seed int) string {
	prefix := fmt.Sprintf("%08x", uint32(seed))
	payload := strings.Join([]string{
		strings.Repeat("a", 32), prefix, strings.Repeat("0", 40), "3", "bin",
	}, ";")
	raw := base64.StdEncoding.EncodeToString([]byte(payload))
	return fmt.Sprintf("2.%d.60.%s.%s", 1792561400+seed, raw, strings.Repeat("0", 32))
}

// verifySolution recomputes the PoW chain from the token and solution.
func verifySolution(t *testing.T, token, solution string) bool {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) < 4 {
		return false
	}
	seed, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	fields := strings.Split(string(raw), ";")
	if len(fields) < 4 {
		return false
	}
	key := int((((seed * int64(testMultipliers[((seed%5)+5)%5])) % 36) + 36) % 36)
	if len(fields[2]) < key+testPrefixLen {
		return false
	}
	cur := fields[2][key : key+testPrefixLen]
	for i, h := range strings.Split(solution, ";") {
		sum := sha256.Sum256([]byte(fields[1] + h))
		d := hex.EncodeToString(sum[:])
		if d[:testPrefixLen] != cur {
			return false
		}
		off := int((((seed * int64(testMultipliers[i%5])) % 60) + 60) % 60)
		cur = d[off : off+testPrefixLen]
	}
	return true
}
