package config

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// defaultMemoryLimit is the soft heap ceiling installed when the operator has
// not set one. The gateway's steady state is a few tens of MiB; a long-running
// instance that reached 2-3 GiB of RSS is not doing useful work with it, it is
// holding garbage. Go's default is to let the heap grow to 2x the live set
// with no ceiling at all, so an idle gateway never gives memory back.
//
// This is a SOFT limit: exceeding it makes the GC run continuously rather
// than failing an allocation, so a genuinely large request set slows down
// instead of dying. Set IL2API_MEMORY_LIMIT=0 to restore Go's default, or to
// an explicit byte/GB value to raise it.
const defaultMemoryLimit = 256 << 20

// memoryLimitFromEnv reads IL2API_MEMORY_LIMIT: a byte count, or a decimal
// size with a K/M/G suffix (G is binary, 1<<30). "0" means "no limit".
func memoryLimitFromEnv(getenv func(string) (string, bool)) (int64, error) {
	raw, ok := getenv("IL2API_MEMORY_LIMIT")
	if !ok {
		return defaultMemoryLimit, nil
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultMemoryLimit, nil
	}
	if v == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'k', 'K':
		mult, v = 1<<10, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1<<20, v[:len(v)-1]
	case 'g', 'G':
		mult, v = 1<<30, v[:len(v)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, errMemoryLimit(raw)
	}
	return n * mult, nil
}

type memoryLimitError string

func (e memoryLimitError) Error() string {
	return "IL2API_MEMORY_LIMIT: want bytes or a size like 256M, got " + strconv.Quote(string(e))
}

func errMemoryLimit(raw string) error { return memoryLimitError(raw) }

// ApplyMemoryLimit installs the soft heap limit. A limit of 0 is a no-op, so
// the Go runtime default applies. It returns the limit it set, for the boot
// log.
func ApplyMemoryLimit(limit int64) int64 {
	if limit > 0 {
		debug.SetMemoryLimit(limit)
	}
	return limit
}
