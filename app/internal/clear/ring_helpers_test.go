package clear

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newUnchallengedServer is an egress the edge does not challenge: every probe
// answers 200 with a session body, and probes counts the requests.
func newUnchallengedServer(t *testing.T, probes *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		io.WriteString(w, `{"ok":true,"token":"1792553057.`+"0000000000000000000000000000000"+`a.`+
			"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}
