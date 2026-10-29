package upstream

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
)

// readBufferSpy records the read size the line pump asks for. net/http's
// response body honours SetReadBuffer; a test reader only needs the method.
type readBufferSpy struct {
	bytes.Reader
	size int
}

func (r *readBufferSpy) SetReadBuffer(n int) { r.size = n }

// oneSSELine builds a single data line of about n bytes, the shape the
// upstream uses for a whole answer or a whole source list.
func oneSSELine(n int) []byte {
	return []byte("data: {\"t\":\"text-delta\",\"s\":\"" + strings.Repeat("a", n) + "\"}\n")
}

// TestLinesReadsInBulk guards the per-read overhead: a 4 KiB read turns every
// TLS record into its own read, and the extra copies per read are what used
// to force the scan buffer to grow on every response.
func TestLinesReadsInBulk(t *testing.T) {
	body := &readBufferSpy{Reader: *bytes.NewReader(oneSSELine(5 << 20))}
	// A body that ends cleanly is reported as io.EOF: that is the success
	// signal for a completed scan, not a failure.
	if err := lines(context.Background(), body, 0, 0, func(string) error { return nil }); err != nil && err != io.EOF {
		t.Fatalf("lines: %v", err)
	}
	if body.size != streamReadBuffer {
		t.Fatalf("SetReadBuffer(%d), want %d", body.size, streamReadBuffer)
	}
}

// TestLinesLargeLineFitsHardCap pins the two properties a single large event
// depends on: a line above the seed still parses (the buffer grows up to
// maxLineBytes), and one above maxLineBytes is an error rather than a silent
// truncation.
func TestLinesLargeLineFitsHardCap(t *testing.T) {
	if err := lines(context.Background(), bytes.NewReader(oneSSELine(2<<20)), 0, 0, func(string) error { return nil }); err != nil && err != io.EOF {
		t.Fatalf("2 MiB line: %v", err)
	}
	over := bytes.NewReader(oneSSELine(maxLineBytes + 64<<10))
	if err := lines(context.Background(), over, 0, 0, func(string) error { return nil }); err == nil || err == io.EOF {
		t.Fatalf("a line above maxLineBytes must fail, got %v", err)
	}
}

// TestLinesSmallLinesStayCompact guards against a seed that would make every
// short response pay for a large buffer: the parser must not retain more than
// a small multiple of the input for ordinary traffic.
func TestLinesSmallLinesStayCompact(t *testing.T) {
	src := []byte("data: {\"t\":\"text-delta\",\"s\":\"hi\"}\n")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 500; i++ {
		if err := lines(context.Background(), bytes.NewReader(src), 0, 0, func(string) error { return nil }); err != nil && err != io.EOF {
			t.Fatalf("lines: %v", err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// 500 short responses must not leave tens of megabytes resident. The
	// threshold is loose on purpose; it only catches per-response retention.
	if grew := after.HeapInuse - before.HeapInuse; grew > 32<<20 {
		t.Fatalf("500 short responses retained %.1f MiB of heap", float64(grew)/float64(1<<20))
	}
}
