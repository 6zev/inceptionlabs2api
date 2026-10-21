package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func token(n int) string {
	nonce := strings.Repeat("a", 32)
	sign := strings.Repeat(string(rune('a'+n%6)), 64)
	return "1792553057." + nonce + "." + sign
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_792_553_057, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeCounter is a fetcher that hands out sequential tokens.
type fakeCounter struct {
	mu   sync.Mutex
	n    int
	fail error
}

func (f *fakeCounter) Fetch(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.n++
	return token(f.n), nil
}

func (f *fakeCounter) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeCounter) SetFail(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

func testConfig(clock *fakeClock) Config {
	return Config{Min: 2, Max: 8, TTL: 30 * time.Minute, Interval: time.Hour, Now: clock.Now}
}

func TestValidateToken(t *testing.T) {
	t.Parallel()
	valid := token(1)
	if !ValidateToken(valid) {
		t.Fatalf("ValidateToken(%q) = false", valid)
	}
	cases := map[string]string{
		"empty":         "",
		"two parts":     "1792553057." + strings.Repeat("a", 32),
		"short nonce":   "1792553057." + strings.Repeat("a", 31) + "." + strings.Repeat("b", 64),
		"long nonce":    "1792553057." + strings.Repeat("a", 33) + "." + strings.Repeat("b", 64),
		"short sign":    "1792553057." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 63),
		"non-hex nonce": "1792553057." + strings.Repeat("z", 32) + "." + strings.Repeat("b", 64),
		"non-hex sign":  "1792553057." + strings.Repeat("a", 32) + "." + strings.Repeat("g", 64),
		"bad timestamp": "abc." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 64),
		"four parts":    "1792553057." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 64) + ".x",
		"too long":      "1792553057." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 65),
	}
	for name, tok := range cases {
		if ValidateToken(tok) {
			t.Errorf("%s: ValidateToken(%q) = true, want false", name, tok)
		}
	}
}

func TestNewClampsConfig(t *testing.T) {
	t.Parallel()
	p := New(Config{Min: 9, Max: 4, TTL: 0, Interval: 0}, (&fakeCounter{}).Fetch)
	defer p.Close()
	st := p.Stats()
	if st.Min != 4 || st.Max != 4 {
		t.Fatalf("stats = %+v, want Min=Max=4", st)
	}
}

func TestGetFetchesUntilMin(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(testConfig(clock), fc.Fetch)
	defer p.Close()

	got := make(map[string]bool)
	for i := 0; i < 2; i++ {
		tok, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got[tok] = true
	}
	if len(got) != 2 {
		t.Fatalf("tokens = %v, want two distinct tokens", got)
	}
	if fc.Count() != 2 {
		t.Fatalf("fetches = %d, want 2", fc.Count())
	}
	if st := p.Stats(); st.Size != 2 {
		t.Fatalf("size = %d, want 2", st.Size)
	}
}

func TestGetFetchFailureIsErrNoToken(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{fail: errors.New("session 429 challenge")}
	p := New(testConfig(clock), fc.Fetch)
	defer p.Close()

	_, err := p.Get(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("err = %v, want the fetch cause", err)
	}
}

func TestGetRespectsMax(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(Config{Min: 8, Max: 2, TTL: time.Hour, Interval: time.Hour, Now: clock.Now}, fc.Fetch)
	defer p.Close()
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fc.Count() != 2 {
		t.Fatalf("fetches = %d, want 2 (Max)", fc.Count())
	}
}

func TestTTLExpiryReplacesToken(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(Config{Min: 1, Max: 2, TTL: 10 * time.Minute, Interval: time.Hour, Now: clock.Now}, fc.Fetch)
	defer p.Close()

	first, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	clock.Advance(9 * time.Minute)
	if st := p.Stats(); st.Size != 1 || st.Expired != 0 {
		t.Fatalf("stats before TTL = %+v, want size 1 expired 0", st)
	}
	clock.Advance(2 * time.Minute)
	if st := p.Stats(); st.Size != 0 || st.Expired != 1 {
		t.Fatalf("stats after TTL = %+v, want size 0 expired 1", st)
	}
	second, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if second == first {
		t.Fatal("expired token was returned again")
	}
	if fc.Count() != 2 {
		t.Fatalf("fetches = %d, want 2", fc.Count())
	}
}

func TestDiscardRemovesOnlyThatToken(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(Config{Min: 2, Max: 3, TTL: time.Hour, Interval: time.Hour, Now: clock.Now}, fc.Fetch)
	defer p.Close()

	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		tok, _ := p.Get(context.Background())
		seen[tok] = true
	}
	var discard string
	for tok := range seen {
		discard = tok
	}
	p.Discard(discard)

	deadline := time.Now().Add(2 * time.Second)
	for {
		var found bool
		for _, tok := range []string{} {
			_ = tok
		}
		_ = found
		st := p.Stats()
		if st.Size == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool not refilled after discard: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The discarded token must never come back.
	for i := 0; i < 4; i++ {
		tok, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if tok == discard {
			t.Fatalf("discarded token returned again")
		}
	}
}

func TestDiscardUnknownTokenIsNoop(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(testConfig(clock), fc.Fetch)
	defer p.Close()
	p.Discard("1792553057." + strings.Repeat("a", 32) + "." + strings.Repeat("b", 64))
	if fc.Count() != 0 {
		t.Fatalf("fetches = %d, want 0", fc.Count())
	}
}

func TestMaintainerRefills(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	cfg := Config{
		Min: 2, Max: 2, TTL: time.Hour, Interval: time.Hour,
		Now:          clock.Now,
		NewTicker:    func(time.Duration) *time.Ticker { return time.NewTicker(time.Hour) },
		OnFetchError: func(error) {},
	}
	p := New(cfg, fc.Fetch)
	defer p.Close()
	// Drive the maintainer directly: its ticker is 1h, too slow for a test,
	// so exercise the same entry point it uses.
	if err := p.grow(context.Background()); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if st := p.Stats(); st.Size != 2 {
		t.Fatalf("size = %d, want 2", st.Size)
	}
}

func TestMaintainerReportsErrors(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{fail: errors.New("boom")}
	errCh := make(chan error, 4)
	cfg := Config{
		Min: 1, Max: 1, TTL: time.Hour, Interval: time.Millisecond,
		Now:          clock.Now,
		OnFetchError: func(err error) { errCh <- err },
	}
	p := New(cfg, fc.Fetch)
	defer p.Close()
	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("maintainer never reported a fetch error")
	}
}

func TestConcurrentGetIsSafe(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(Config{Min: 2, Max: 8, TTL: time.Hour, Interval: time.Hour, Now: clock.Now}, fc.Fetch)
	defer p.Close()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Get(context.Background()); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()
	if fc.Count() > 8 {
		t.Fatalf("fetches = %d, want <= Max 8", fc.Count())
	}
	if st := p.Stats(); st.Size < 1 || st.Size > 8 {
		t.Fatalf("size = %d, want 1..8", st.Size)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	p := New(testConfig(newClock()), (&fakeCounter{}).Fetch)
	p.Close()
	p.Close()
}

func TestMinZeroServesOnDemand(t *testing.T) {
	t.Parallel()
	clock := newClock()
	fc := &fakeCounter{}
	p := New(Config{Min: 0, Max: 4, TTL: time.Hour, Interval: time.Hour, Now: clock.Now}, fc.Fetch)
	defer p.Close()

	if st := p.Stats(); st.Size != 0 {
		t.Fatalf("fresh lazy pool size = %d, want 0", st.Size)
	}
	tok, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get on a Min=0 pool: %v", err)
	}
	if tok == "" || fc.Count() != 1 {
		t.Fatalf("tok = %q fetches = %d, want one on-demand token", tok, fc.Count())
	}
	if st := p.Stats(); st.Size != 1 {
		t.Fatalf("size = %d, want 1", st.Size)
	}
}
