package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics owns this server's Prometheus registry + instruments. Each Server gets
// its OWN registry (NewServer runs per test), so there's no global
// double-registration panic. /metrics serves it via promhttp; a Prometheus Operator
// ServiceMonitor (deploy/helm) scrapes it and the Grafana dashboard reads it.
//
// The latency histogram is the core RED signal: bucketed observations let
// Prometheus compute true p50/p95/p99 (histogram_quantile) and request rate +
// error rate (by the status label) — none of which a sum/count counter can give.
type metrics struct {
	reg     *prometheus.Registry
	httpDur *prometheus.HistogramVec
}

// poolStats is the snapshot the pgxpool collector exposes (read at scrape time).
type poolStats struct{ acquired, idle, total, max int32 }

func newMetrics(poolStat func() poolStats) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		httpDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "bank0",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency by method, route template and status code.",
			// API-tuned buckets: sub-ms reads through slow money moves up to a 10s tail.
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route", "status"}),
	}
	m.reg.MustRegister(
		m.httpDur,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newPoolCollector(poolStat),
	)
	return m
}

// observe records one request. route is the mux route TEMPLATE (e.g.
// /transfers/{id}), not the concrete path, so account/transfer ids don't explode
// label cardinality.
func (m *metrics) observe(method, route string, status int, d time.Duration) {
	m.httpDur.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}

// poolCollector exposes pgxpool saturation as a gauge sampled at scrape time —
// the signal that tells you the connection pool (small by default) is the
// bottleneck under load. Implemented as a Collector so the value is always live.
type poolCollector struct {
	stat func() poolStats
	desc *prometheus.Desc
}

func newPoolCollector(stat func() poolStats) *poolCollector {
	return &poolCollector{
		stat: stat,
		desc: prometheus.NewDesc(
			"bank0_db_pool_conns",
			"Postgres connection-pool connections by state (live pgxpool.Stat).",
			[]string{"state"}, nil,
		),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stat()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.acquired), "acquired")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.idle), "idle")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.total), "total")
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s.max), "max")
}

// Metrics serves the Prometheus exposition for this server's registry.
func (s *Server) Metrics(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

// routeTemplate returns the matched mux route template for cardinality-safe
// labeling, or "unmatched" for 404/405 (no route).
func routeTemplate(r *http.Request) string {
	if cur := mux.CurrentRoute(r); cur != nil {
		if tmpl, err := cur.GetPathTemplate(); err == nil {
			return tmpl
		}
	}
	return "unmatched"
}
