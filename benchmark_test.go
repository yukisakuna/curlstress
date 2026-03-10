package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func BenchmarkRequestFactoryNewRequestGet(b *testing.B) {
	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    "http://example.com/api/items?q=1",
		Headers: http.Header{
			"User-Agent": []string{"curlstress-bench"},
			"X-Test":     []string{"1"},
		},
	}
	factory, err := spec.newRequestFactory()
	if err != nil {
		b.Fatalf("newRequestFactory: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, err := factory.NewRequest(context.Background())
			if err != nil {
				b.Fatalf("NewRequest: %v", err)
			}
			if req.URL == nil {
				b.Fatal("request URL is nil")
			}
			if req.Body != nil && req.Body != http.NoBody {
				_ = req.Body.Close()
			}
		}
	})
}

func BenchmarkLoopbackRoundTripGET(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
		Headers: http.Header{
			"User-Agent": []string{"curlstress-bench"},
		},
	}
	client := newHTTPClient(spec, LoadTestConfig{
		Workers:        128,
		RequestTimeout: 5 * time.Second,
		HTTPTimeout:    5 * time.Second,
	})
	factory, err := spec.newRequestFactory()
	if err != nil {
		b.Fatalf("newRequestFactory: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		drainBuf := make([]byte, 32*1024)
		for pb.Next() {
			req, err := factory.NewRequest(context.Background())
			if err != nil {
				b.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				b.Fatalf("Do: %v", err)
			}
			_, _ = io.CopyBuffer(io.Discard, resp.Body, drainBuf)
			_ = resp.Body.Close()
		}
	})
}

func BenchmarkLoopbackRoundTripPOST(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodPost,
		URL:    server.URL,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Test":       []string{"bench"},
		},
		Body: []byte(`{"hello":"world"}`),
	}
	client := newHTTPClient(spec, LoadTestConfig{
		Workers:        128,
		RequestTimeout: 5 * time.Second,
		HTTPTimeout:    5 * time.Second,
	})
	factory, err := spec.newRequestFactory()
	if err != nil {
		b.Fatalf("newRequestFactory: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		drainBuf := make([]byte, 32*1024)
		for pb.Next() {
			req, err := factory.NewRequest(context.Background())
			if err != nil {
				b.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				b.Fatalf("Do: %v", err)
			}
			_, _ = io.CopyBuffer(io.Discard, resp.Body, drainBuf)
			_ = resp.Body.Close()
		}
	})
}
