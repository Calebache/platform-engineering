package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	registry *prometheus.Registry
	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mcp_requests_total",
			Help: "MCP requests by method, tool/resource/prompt name and outcome (allowed, denied, error, tool_error).",
		}, []string{"method", "name", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mcp_request_duration_seconds",
			Help:    "MCP request latency by method, including the policy check.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method"}),
	}
	m.registry.MustRegister(m.calls, m.duration,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
