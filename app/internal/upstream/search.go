package upstream

// Search wire types for /api/search-lab/*. The NDJSON event vocabulary and
// field names are the ones the /search playground consumes
// (docs/RESEARCH-websearch.md §4.3).

// SearchConfig is the tunable part of a search-lab run. Values are sent
// verbatim; validation and clamping are the server's job.
type SearchConfig struct {
	Fanout   int    `json:"fanout"`
	Depth    int    `json:"depth"`
	KeepTopK int    `json:"keepTopK"`
	Effort   string `json:"effort"`
}

// SearchRequest is the POST /api/search-lab/run body.
type SearchRequest struct {
	Query    string       `json:"query"`
	Config   SearchConfig `json:"config"`
	Timezone string       `json:"timezone"`
	Language string       `json:"language"`
}

// SearchSource is one entry of a docs event's sources array. The upstream
// publishes no snippet, so none is modelled.
type SearchSource struct {
	Title string  `json:"title"`
	URL   string  `json:"url"`
	Score float64 `json:"score"`
}

// SearchResult is what a completed run yields: the rewritten queries seen so
// far, the final ranked source list, and — when the run reached synthesis —
// the upstream's content-grounded answer. The answer's [n] citation markers
// align with the sources order (the docs event's cite_index starts at 1 in
// exactly the order Sources preserves).
type SearchResult struct {
	Queries []string
	Sources []SearchSource
	Answer  string
}

// SearchConfigResponse is the GET /api/search-lab/config response. Only the
// fields the gateway acts on are modelled.
type SearchConfigResponse struct {
	RetrievalAvailable bool `json:"retrievalAvailable"`
	Defaults           struct {
		Fanout   int    `json:"fanout"`
		Depth    int    `json:"depth"`
		KeepTopK int    `json:"keepTopK"`
		Effort   string `json:"effort"`
	} `json:"defaults"`
	Limits struct {
		MinFanout int `json:"minFanout"`
		MaxFanout int `json:"maxFanout"`
		MinDepth  int `json:"minDepth"`
		MaxDepth  int `json:"maxDepth"`
	} `json:"limits"`
}

// searchEvent is one NDJSON line. Unknown types and fields are ignored.
type searchEvent struct {
	Type    string         `json:"t"`
	Kind    string         `json:"kind"`
	Queries []string       `json:"queries"`
	Sources []SearchSource `json:"sources"`
	Message string         `json:"message"`
	Run     int            `json:"run"`
	Of      int            `json:"of"`
	// Answer is the run_done event's full synthesized answer.
	Answer string `json:"answer"`
	// D is one answer_delta event's incremental text.
	D string `json:"d"`
}

// searchEventTypes the collector reacts to. call_start (synthesize included)
// and rounds are NOT terminators: the measured 2026-10-22 capture shows the
// content-grounded answer phase follows them, ending at run_done.
const (
	searchEventQueries     = "queries"
	searchEventDocs        = "docs"
	searchEventError       = "error"
	searchEventCallStart   = "call_start"
	searchEventRunDone     = "run_done"
	searchEventAnswerDelta = "answer_delta"
)
