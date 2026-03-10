package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLoadTestHitsServer(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Errorf("x-test = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
		}
		if got := string(body); got != `{"ok":true}` {
			t.Errorf("body = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodPost,
		URL:    server.URL,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Test":       []string{"yes"},
		},
		Body: []byte(`{"ok":true}`),
	}

	result, err := RunLoadTest(spec, LoadTestConfig{
		Duration:         250 * time.Millisecond,
		Workers:          4,
		RPS:              20,
		QueueSize:        16,
		RequestTimeout:   2 * time.Second,
		HTTPTimeout:      2 * time.Second,
		ProgressInterval: 0,
	}, nil)
	if err != nil {
		t.Fatalf("RunLoadTest returned error: %v", err)
	}

	if result.Started == 0 {
		t.Fatalf("expected at least one dispatched request")
	}
	if result.Errors != 0 {
		t.Fatalf("errors = %d", result.Errors)
	}
	if result.StatusClasses[3] != 0 || result.StatusClasses[4] != 0 || result.StatusClasses[5] != 0 {
		t.Fatalf("unexpected status classes: %+v", result.StatusClasses)
	}
	if result.StatusCodes[http.StatusCreated] == 0 {
		t.Fatalf("201 count = %d", result.StatusCodes[http.StatusCreated])
	}
	if got := hits.Load(); uint64(got) != result.Completed {
		t.Fatalf("server hits = %d, completed = %d", got, result.Completed)
	}
}

func TestRunLoadTestLowRPSStillDispatches(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
		Headers: http.Header{
			"X-Test": []string{"low-rps"},
		},
	}

	result, err := RunLoadTest(spec, LoadTestConfig{
		Duration:         time.Second,
		Workers:          1,
		RPS:              1,
		QueueSize:        8,
		RequestTimeout:   time.Second,
		HTTPTimeout:      time.Second,
		ProgressInterval: 0,
	}, nil)
	if err != nil {
		t.Fatalf("RunLoadTest returned error: %v", err)
	}

	if result.Started == 0 {
		t.Fatalf("expected at least one dispatched request at low RPS")
	}
	if got := hits.Load(); got == 0 {
		t.Fatalf("expected server to be hit at least once")
	}
}

func TestRunLoadTestWaitsForInflightToFinish(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
	}

	result, err := RunLoadTest(spec, LoadTestConfig{
		Duration:         50 * time.Millisecond,
		Workers:          1,
		RPS:              0,
		RequestTimeout:   time.Second,
		HTTPTimeout:      time.Second,
		ProgressInterval: 0,
	}, nil)
	if err != nil {
		t.Fatalf("RunLoadTest returned error: %v", err)
	}

	if result.Started == 0 {
		t.Fatalf("expected at least one request to start")
	}
	if result.Started != result.Completed+result.Errors {
		t.Fatalf("final counts inconsistent: started=%d completed=%d errors=%d", result.Started, result.Completed, result.Errors)
	}
	if hits.Load() == 0 {
		t.Fatalf("expected server to be hit")
	}
}

func TestRunLoadTestTLSRawBackend(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("tls-ok"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method:   http.MethodGet,
		URL:      server.URL,
		Insecure: true,
	}

	if got := detectLoadBackend(spec); got != loadBackendRawH1 {
		t.Fatalf("backend = %s, want %s", got, loadBackendRawH1)
	}

	result, err := RunLoadTest(spec, LoadTestConfig{
		Duration:         150 * time.Millisecond,
		Workers:          2,
		RPS:              10,
		RequestTimeout:   time.Second,
		HTTPTimeout:      time.Second,
		ProgressInterval: 0,
	}, nil)
	if err != nil {
		t.Fatalf("RunLoadTest returned error: %v", err)
	}

	if result.Completed == 0 {
		t.Fatalf("expected completed requests")
	}
	if result.StatusCodes[http.StatusAccepted] == 0 {
		t.Fatalf("202 count = %d", result.StatusCodes[http.StatusAccepted])
	}
	if hits.Load() == 0 {
		t.Fatalf("expected server to be hit")
	}
}

func TestRunLoadTestChunkedRawBackend(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("response writer does not implement Flusher")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
		flusher.Flush()
		_, _ = w.Write([]byte("world"))
	}))
	defer server.Close()

	spec := RequestSpec{
		Method: http.MethodGet,
		URL:    server.URL,
	}

	if got := detectLoadBackend(spec); got != loadBackendRawH1 {
		t.Fatalf("backend = %s, want %s", got, loadBackendRawH1)
	}

	result, err := RunLoadTest(spec, LoadTestConfig{
		Duration:         150 * time.Millisecond,
		Workers:          2,
		RPS:              10,
		RequestTimeout:   time.Second,
		HTTPTimeout:      time.Second,
		ProgressInterval: 0,
	}, nil)
	if err != nil {
		t.Fatalf("RunLoadTest returned error: %v", err)
	}

	if result.Completed == 0 {
		t.Fatalf("expected completed requests")
	}
	if result.StatusCodes[http.StatusOK] == 0 {
		t.Fatalf("200 count = %d", result.StatusCodes[http.StatusOK])
	}
	if hits.Load() == 0 {
		t.Fatalf("expected server to be hit")
	}
}
