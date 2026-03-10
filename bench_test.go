package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func BenchmarkPreparedRequestGET(b *testing.B) {
	b.ReportAllocs()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
		Headers: http.Header{
			"User-Agent": []string{"bench"},
		},
	}

	factory, err := spec.newRequestFactory()
	if err != nil {
		b.Fatalf("newRequestFactory: %v", err)
	}

	client := newHTTPClient(spec, LoadTestConfig{
		Workers:        64,
		RequestTimeout: 5 * time.Second,
		HTTPTimeout:    5 * time.Second,
	})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 4*1024)
		for pb.Next() {
			req, err := factory.NewRequest(context.Background())
			if err != nil {
				b.Fatalf("NewRequest: %v", err)
			}

			resp, err := client.Do(req)
			if err != nil {
				b.Fatalf("Do: %v", err)
			}
			if _, err := io.CopyBuffer(io.Discard, resp.Body, buf); err != nil {
				_ = resp.Body.Close()
				b.Fatalf("CopyBuffer: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				b.Fatalf("Close: %v", err)
			}
		}
	})
}

func BenchmarkRunLoadTestLoopbackGET(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		result, err := RunLoadTest(spec, LoadTestConfig{
			Duration:         200 * time.Millisecond,
			Workers:          64,
			RPS:              0,
			RequestTimeout:   5 * time.Second,
			HTTPTimeout:      5 * time.Second,
			ProgressInterval: 0,
		}, nil)
		if err != nil {
			b.Fatalf("RunLoadTest: %v", err)
		}
		b.ReportMetric(float64(result.Completed)/0.2, "req/s")
	}
}

func BenchmarkRunLoadTestLoopbackPOSTRPS(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodPost,
		URL:    server.URL,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: []byte(`{"ok":true}`),
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		result, err := RunLoadTest(spec, LoadTestConfig{
			Duration:         200 * time.Millisecond,
			Workers:          32,
			RPS:              1000,
			RequestTimeout:   5 * time.Second,
			HTTPTimeout:      5 * time.Second,
			ProgressInterval: 0,
		}, nil)
		if err != nil {
			b.Fatalf("RunLoadTest: %v", err)
		}
		b.ReportMetric(float64(result.Completed)/0.2, "req/s")
	}
}
