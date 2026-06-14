package api

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// metrics holds lightweight, dependency-free counters rendered in Prometheus text
// format at /metrics. Request RED signals (rate/errors/duration) come from
// requestLogger; DB pool saturation is read live from pgxpool.Stat() at scrape
// time. A full client_golang histogram + OpenTelemetry tracing is the natural
// follow-up — this gives operators the highest-value time series with no new dep.
type metrics struct {
	req2xx      atomic.Int64
	req3xx      atomic.Int64
	req4xx      atomic.Int64
	req5xx      atomic.Int64
	reqTotal    atomic.Int64
	durSumMicro atomic.Int64 // Σ request durations in microseconds (avg = sum/total)
}

func (m *metrics) observe(status int, d time.Duration) {
	m.reqTotal.Add(1)
	m.durSumMicro.Add(d.Microseconds())
	switch {
	case status >= 500:
		m.req5xx.Add(1)
	case status >= 400:
		m.req4xx.Add(1)
	case status >= 300:
		m.req3xx.Add(1)
	case status >= 200:
		m.req2xx.Add(1)
	}
}

// Metrics renders a minimal Prometheus exposition (RED counters + DB pool gauges).
// Registered on the public router alongside /health; restrict at the network layer
// if the instance is internet-facing.
func (s *Server) Metrics(w http.ResponseWriter, r *http.Request) {
	m := &s.metrics
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprint(w, "# HELP bank0_http_requests_total Total HTTP requests by status class.\n")
	fmt.Fprint(w, "# TYPE bank0_http_requests_total counter\n")
	fmt.Fprintf(w, "bank0_http_requests_total{class=\"2xx\"} %d\n", m.req2xx.Load())
	fmt.Fprintf(w, "bank0_http_requests_total{class=\"3xx\"} %d\n", m.req3xx.Load())
	fmt.Fprintf(w, "bank0_http_requests_total{class=\"4xx\"} %d\n", m.req4xx.Load())
	fmt.Fprintf(w, "bank0_http_requests_total{class=\"5xx\"} %d\n", m.req5xx.Load())

	fmt.Fprint(w, "# HELP bank0_http_request_duration_seconds_sum Cumulative request duration.\n")
	fmt.Fprint(w, "# TYPE bank0_http_request_duration_seconds_sum counter\n")
	fmt.Fprintf(w, "bank0_http_request_duration_seconds_sum %f\n", float64(m.durSumMicro.Load())/1e6)
	fmt.Fprint(w, "# HELP bank0_http_request_duration_seconds_count Requests observed (avg = sum/count).\n")
	fmt.Fprint(w, "# TYPE bank0_http_request_duration_seconds_count counter\n")
	fmt.Fprintf(w, "bank0_http_request_duration_seconds_count %d\n", m.reqTotal.Load())

	st := s.pg.Pool.Stat()
	fmt.Fprint(w, "# HELP bank0_db_pool_conns Postgres pool connections by state.\n")
	fmt.Fprint(w, "# TYPE bank0_db_pool_conns gauge\n")
	fmt.Fprintf(w, "bank0_db_pool_conns{state=\"acquired\"} %d\n", st.AcquiredConns())
	fmt.Fprintf(w, "bank0_db_pool_conns{state=\"idle\"} %d\n", st.IdleConns())
	fmt.Fprintf(w, "bank0_db_pool_conns{state=\"total\"} %d\n", st.TotalConns())
	fmt.Fprintf(w, "bank0_db_pool_conns{state=\"max\"} %d\n", st.MaxConns())
}
