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
// far and the final ranked source list.
type SearchResult struct {
	Queries []string
	Sources []SearchSource
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
}

// searchEventTypes the collector reacts to. Rounds, call_start, and run_done
// are only used to recognise the end of the retrieval phase.
const (
	searchEventQueries   = "queries"
	searchEventDocs      = "docs"
	searchEventError     = "error"
	searchEventRounds    = "rounds"
	searchEventCallStart = "call_start"
	searchEventRunDone   = "run_done"
	searchKindSynthesize = "synthesize"
)
