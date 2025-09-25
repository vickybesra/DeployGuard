package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	rand.Seed(time.Now().UnixNano())
}

func main() {
	// Get MODE from environment variable
	envMode := os.Getenv("MODE")
	if envMode != "" {
		fmt.Printf("Running in %s mode\n", envMode)
	}

	http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := "200"

		// Check header first, then fall back to environment variable
		mode := r.Header.Get("X-Mode")
		if mode == "" {
			mode = envMode
		}

		// Simulate behavior based on mode
		switch mode {
		case "error":
			// Always return 500 errors
			status = "500"
			w.WriteHeader(500)
			w.Write([]byte("Simulated error"))
		case "slow":
			// High latency (600ms, above typical 500ms threshold)
			time.Sleep(600 * time.Millisecond)
			w.WriteHeader(200)
			w.Write([]byte("Slow response"))
		default:
			// Normal mode with 1% random errors
			if rand.Float32() < 0.01 {
				status = "500"
				w.WriteHeader(500)
				w.Write([]byte("Random error"))
			} else {
				w.WriteHeader(200)
				w.Write([]byte("OK"))
			}
		}

		duration := time.Since(start).Seconds()
		httpRequestDuration.WithLabelValues().Observe(duration)
		httpRequestsTotal.WithLabelValues(status).Inc()
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("healthy"))
	})

	fmt.Println("Server starting on :8080")
	http.ListenAndServe(":8080", nil)
}
