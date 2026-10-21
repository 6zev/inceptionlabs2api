package upstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowBody emits lines with controlled timing so the idle watchdog can be
// exercised deterministically.
type slowBody struct {
	mu     sync.Mutex
	lines  []string
	delays []time.Duration
	i      int
	closed chan struct{}
}

func newSlowBody(lines []string, delays []time.Duration) *slowBody {
	return &slowBody{lines: lines, delays: delays, closed: make(chan struct{})}
}

func (b *slowBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.i >= len(b.lines) {
		b.mu.Unlock()
		return 0, ioEOF
	}
	line := b.lines[b.i]
	delay := time.Duration(0)
	if b.i < len(b.delays) {
		delay = b.delays[b.i]
	}
	b.i++
	b.mu.Unlock()
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-b.closed:
			t.Stop()
			return 0, errors.New("closed")
		}
	}
	copy(p, line)
	return len(line), nil
}

func (b *slowBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

var ioEOF = errors.New("EOF")

func sse(event string) string { return "data: " + event + "\n\n" }

func TestReaderEmitsEventsAndStopsAtDone(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(
		sse(`{"type":"reasoning-start","id":"reasoning-0"}`) +
			sse(`{"type":"reasoning-delta","id":"reasoning-0","delta":"Think."}`) +
			": comment\n\n" +
			sse(`{"type":"reasoning-end","id":"reasoning-0"}`) +
			sse(`{"type":"text-start","id":"text-0"}`) +
			sse(`{"type":"text-delta","id":"text-0","delta":"Hello"}`) +
			sse(`{"type":"text-end","id":"text-0"}`) +
			"data: [DONE]\n\n" +
			sse(`{"type":"text-delta","id":"text-0","delta":"after done"}`),
	)
	var seen []string
	err := NewReader(body, time.Second, nil).Read(context.Background(), func(ev Event) error {
		seen = append(seen, ev.Type)
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"reasoning-start", "reasoning-delta", "reasoning-end", "text-start", "text-delta", "text-end"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", seen, want)
	}
}

func TestReaderSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(
		"data: {not json}\n\n" +
			sse(`{"no":"type"}`) +
			sse(`{"type":"text-delta","id":"text-0","delta":"ok"}`) +
			"data: [DONE]\n\n",
	)
	var malformed []string
	var deltas int
	err := NewReader(body, time.Second, func(line string) { malformed = append(malformed, line) }).Read(context.Background(), func(ev Event) error {
		if ev.Type == EventTextDelta {
			deltas++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(malformed) != 2 {
		t.Fatalf("malformed = %v, want 2", malformed)
	}
	if deltas != 1 {
		t.Fatalf("deltas = %d, want 1", deltas)
	}
}

func TestReaderTruncatedWithoutDone(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(sse(`{"type":"text-delta","id":"text-0","delta":"partial"}`))
	err := NewReader(body, time.Second, nil).Read(context.Background(), func(Event) error { return nil })
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
}

func TestReaderEmptyStreamIsTruncated(t *testing.T) {
	t.Parallel()
	err := NewReader(strings.NewReader(""), time.Second, nil).Read(context.Background(), func(Event) error { return nil })
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
}

func TestReaderIdleWatchdog(t *testing.T) {
	t.Parallel()
	body := newSlowBody([]string{
		sse(`{"type":"text-delta","id":"text-0","delta":"first"}`),
		sse(`{"type":"text-delta","id":"text-0","delta":"never"}`),
	}, []time.Duration{0, 2 * time.Second})
	defer body.Close()

	start := time.Now()
	err := NewReader(body, 150*time.Millisecond, nil).Read(context.Background(), func(Event) error { return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if elapsed > time.Second {
		t.Fatalf("watchdog took %s, want ~150ms", elapsed)
	}
}

func TestReaderIdleResetsOnEveryLine(t *testing.T) {
	t.Parallel()
	// Ten lines, each 80ms apart, with a 200ms idle window: a wall-clock cap
	// would fire, a sliding timer must not.
	lines := make([]string, 10)
	delays := make([]time.Duration, 10)
	for i := range lines {
		lines[i] = sse(`{"type":"text-delta","id":"text-0","delta":"x"}`)
		delays[i] = 80 * time.Millisecond
	}
	lines = append(lines, "data: [DONE]\n\n")
	body := newSlowBody(lines, delays)
	defer body.Close()

	count := 0
	err := NewReader(body, 200*time.Millisecond, nil).Read(context.Background(), func(Event) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if count != 10 {
		t.Fatalf("events = %d, want 10", count)
	}
}

func TestReaderContextCancel(t *testing.T) {
	t.Parallel()
	body := newSlowBody([]string{sse(`{"type":"text-delta","id":"text-0","delta":"x"}`)}, []time.Duration{5 * time.Second})
	defer body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := NewReader(body, 5*time.Second, nil).Read(ctx, func(Event) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not interrupt the read promptly")
	}
}

func TestReaderCallbackErrorStops(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(
		sse(`{"type":"text-delta","id":"text-0","delta":"x"}`) +
			sse(`{"type":"text-delta","id":"text-0","delta":"y"}`) +
			"data: [DONE]\n\n",
	)
	stop := errors.New("stop")
	var seen int
	err := NewReader(body, time.Second, nil).Read(context.Background(), func(Event) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want stop", err)
	}
	if seen != 1 {
		t.Fatalf("seen = %d, want 1", seen)
	}
}

func TestEventAccessors(t *testing.T) {
	t.Parallel()
	snapshot := Event{Type: EventTextStart, Snapshot: "whole block"}
	if snapshot.ContentText() != "whole block" {
		t.Fatalf("snapshot text lost: %q", snapshot.ContentText())
	}
	delta := Event{Type: EventTextDelta, Delta: "more"}
	if delta.ContentText() != "more" {
		t.Fatalf("delta text = %q", delta.ContentText())
	}
	if (Event{Type: EventTextEnd}).ContentText() != "" {
		t.Fatal("text-end should contribute nothing")
	}
	if r := (Event{Type: EventReasoningDelta, Delta: "plan"}).Reasoning(); r != "plan" {
		t.Fatalf("reasoning = %q", r)
	}
	if (Event{Type: EventReasoningStart}).Reasoning() != "" {
		t.Fatal("reasoning-start should contribute nothing")
	}
}
