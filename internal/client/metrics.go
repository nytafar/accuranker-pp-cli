// Hand-authored: per-process client request metrics.
//
// PATCH(amend-2026-07-07: run report — spec F1). The warehouse run report
// emitted by `dump` and `mirror` must state requests_made and
// rate_limit_hits. The counters live on Client (incremented inside do() in
// client.go); this file exposes the read side so hand-authored commands can
// snapshot them without touching the generated request path further.
package client

// Metrics returns the number of wire requests sent and 429 responses
// received by this client instance since construction. Cache-served reads
// are not counted as requests. Safe for concurrent use.
func (c *Client) Metrics() (requests, rateLimitHits int64) {
	return c.requestCount.Load(), c.rateLimitHits.Load()
}
