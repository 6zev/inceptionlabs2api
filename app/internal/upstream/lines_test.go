package upstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The pump delivers every line in order, then reports a clean end as io.EOF
// so the family can decide what a close means (chat: truncation; search:
// complete if docs were already seen).
func TestLinesDeliversEveryLineThenEOF(t *testing.T) {
	t.Parallel()
	var got []string
	err := lines(context.Background(), strings.NewReader("a\n\nb\nlast"), time.Second, 0, func(line string) error {
		got = append(got, line)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if want := "a||b|last"; strings.Join(got, "|") != want {
		t.Fatalf("lines = %q, want %q (blank lines included)", got, want)
	}
}

// A callback stop ends the pump with that sentinel and does not read on.
func TestLinesCallbackStop(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	err := lines(context.Background(), strings.NewReader("1\n2\n3\n"), time.Second, 0, func(string) error {
		n.Add(1)
		return errStopLines
	})
	if !errors.Is(err, errStopLines) {
		t.Fatalf("err = %v, want errStopLines", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("callback ran %d times after stopping, want 1", got)
	}
}

// The watchdog is sliding: every line, including a blank separator, resets it.
func TestLinesIdleIsSliding(t *testing.T) {
	t.Parallel()
	delays := []time.Duration{40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	body := newSlowBody([]string{"a\n", "\n", "b\n", "\n", "c\n"}, delays)
	start := time.Now()
	err := lines(context.Background(), body, 100*time.Millisecond, 0, func(string) error { return nil })
	// slowBody's close is its own error, not io.EOF: the point is that the
	// pump reached the end instead of timing out.
	if errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v; the idle window should have been reset by each line", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("pump finished in %s, want it to have outlived several idle windows", elapsed)
	}
}

// No line for the whole window is ErrIdleTimeout.
func TestLinesIdleExpires(t *testing.T) {
	t.Parallel()
	body := newSlowBody([]string{"first\n", "second\n"}, []time.Duration{0, 10 * time.Second})
	defer body.Close()
	start := time.Now()
	err := lines(context.Background(), body, 80*time.Millisecond, 0, func(string) error { return nil })
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("idle expiry took %s", elapsed)
	}
}

// The hard cap fires independently of the idle window: a body that emits one
// line per idle period still ends.
func TestLinesHardCap(t *testing.T) {
	t.Parallel()
	body := newSlowBody(
		[]string{"keepalive\n", "keepalive\n", "keepalive\n", "keepalive\n", "keepalive\n"},
		[]time.Duration{40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond},
	)
	defer body.Close()
	start := time.Now()
	err := lines(context.Background(), body, 60*time.Millisecond, 150*time.Millisecond, func(string) error { return nil })
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout from the hard cap", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("hard cap took %s", elapsed)
	}
}

// Cancellation propagates as the context's error, not as a timeout.
func TestLinesContextCancel(t *testing.T) {
	t.Parallel()
	body := newSlowBody([]string{"x\n"}, []time.Duration{10 * time.Second})
	defer body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := lines(ctx, body, time.Second, 0, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A read error is wrapped with the line-pump prefix, so families can log it
// without re-deriving the source.
func TestLinesReadError(t *testing.T) {
	t.Parallel()
	err := lines(context.Background(), errReader{}, time.Second, 0, func(string) error { return nil })
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a wrapped read error", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
