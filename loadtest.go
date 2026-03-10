package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	workerStatsFlushEvery    = 256
	workerStatsFlushInterval = 500 * time.Millisecond
)

type LoadTestConfig struct {
	Duration         time.Duration
	Workers          int
	RPS              int
	QueueSize        int
	RequestTimeout   time.Duration
	HTTPTimeout      time.Duration
	ProgressInterval time.Duration
}

type StatsSnapshot struct {
	Started       uint64
	Completed     uint64
	Errors        uint64
	BytesRead     uint64
	LatencyCount  uint64
	AvgLatency    time.Duration
	MinLatency    time.Duration
	MaxLatency    time.Duration
	Elapsed       time.Duration
	StatusCodes   map[int]uint64
	StatusClasses map[int]uint64
}

type LoadTestResult struct {
	StatsSnapshot
	CompletedPerSec float64
}

type requestStats struct {
	mu           sync.RWMutex
	started      uint64
	completed    uint64
	errors       uint64
	bytesRead    uint64
	latencyTotal uint64
	latencyCount uint64
	minLatency   uint64
	maxLatency   uint64
	statusCodes  [600]uint64
}

type workerStats struct {
	started      uint64
	completed    uint64
	errors       uint64
	bytesRead    uint64
	latencyTotal uint64
	latencyCount uint64
	minLatency   uint64
	maxLatency   uint64
	statusCodes  [600]uint64
	dirtyCodes   []int
	updates      int
}

type workerPacer struct {
	interval time.Duration
	next     time.Time
}

func RunLoadTest(spec RequestSpec, cfg LoadTestConfig, progress func(StatsSnapshot)) (LoadTestResult, error) {
	return RunLoadTestWithContext(context.Background(), spec, cfg, progress)
}

func RunLoadTestWithContext(ctx context.Context, spec RequestSpec, cfg LoadTestConfig, progress func(StatsSnapshot)) (LoadTestResult, error) {
	if cfg.Duration <= 0 {
		return LoadTestResult{}, fmt.Errorf("duration must be > 0")
	}
	if cfg.Workers < 1 {
		return LoadTestResult{}, fmt.Errorf("workers must be > 0")
	}
	if cfg.QueueSize < 0 {
		return LoadTestResult{}, fmt.Errorf("queue size must be >= 0")
	}
	if cfg.RequestTimeout < 0 {
		return LoadTestResult{}, fmt.Errorf("request timeout must be >= 0")
	}
	if cfg.HTTPTimeout < 0 {
		return LoadTestResult{}, fmt.Errorf("http timeout must be >= 0")
	}
	if cfg.ProgressInterval < 0 {
		return LoadTestResult{}, fmt.Errorf("progress interval must be >= 0")
	}

	if rawPlan, ok, err := prepareRawBackend(spec); err != nil {
		return LoadTestResult{}, err
	} else if ok {
		return runRawLoadTestWithContext(ctx, rawPlan, cfg, progress)
	}

	client := newHTTPClient(spec, cfg)
	requestFactory := spec.factory
	if requestFactory == nil {
		var err error
		requestFactory, err = spec.newRequestFactory()
		if err != nil {
			return LoadTestResult{}, fmt.Errorf("prepare request factory: %w", err)
		}
	}

	stats := newRequestStats()
	launchCtx, cancelLaunch := context.WithTimeout(ctx, cfg.Duration)
	defer cancelLaunch()

	startedAt := time.Now()
	workers := effectiveWorkers(cfg.Workers, cfg.RPS)
	var workerWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		pacer := newWorkerPacer(splitRPS(cfg.RPS, workers, i))
		go func(p *workerPacer) {
			defer workerWG.Done()
			runWorker(launchCtx, client, requestFactory, p, stats)
		}(pacer)
	}

	var progressWG sync.WaitGroup
	progressStop := make(chan struct{})
	if progress != nil && cfg.ProgressInterval > 0 {
		progressWG.Add(1)
		go func() {
			defer progressWG.Done()
			ticker := time.NewTicker(cfg.ProgressInterval)
			defer ticker.Stop()
			for {
				select {
				case <-progressStop:
					return
				case <-ticker.C:
					progress(stats.snapshot(time.Since(startedAt)))
				}
			}
		}()
	}

	workerWG.Wait()
	close(progressStop)
	progressWG.Wait()

	snapshot := stats.snapshot(time.Since(startedAt))
	result := LoadTestResult{StatsSnapshot: snapshot}
	if snapshot.Elapsed > 0 {
		result.CompletedPerSec = float64(snapshot.Completed) / snapshot.Elapsed.Seconds()
	}
	return result, nil
}

func newHTTPClient(spec RequestSpec, cfg LoadTestConfig) *http.Client {
	dialTimeout := minNonZero(cfg.RequestTimeout, 10*time.Second)
	if cfg.HTTPTimeout > 0 && cfg.HTTPTimeout < dialTimeout {
		dialTimeout = cfg.HTTPTimeout
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          maxInt(cfg.Workers*8, 512),
		MaxIdleConnsPerHost:   maxInt(cfg.Workers*4, 256),
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: cfg.HTTPTimeout,
		ExpectContinueTimeout: 0,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: spec.Insecure,
		ClientSessionCache: tls.NewLRUClientSessionCache(maxInt(cfg.Workers*4, 256)),
	}

	client := &http.Client{Transport: transport}
	if cfg.RequestTimeout > 0 {
		client.Timeout = cfg.RequestTimeout
	} else if cfg.HTTPTimeout > 0 {
		client.Timeout = cfg.HTTPTimeout
	}
	if !spec.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func runWorker(launchCtx context.Context, client *http.Client, requestFactory *requestFactory, pacer *workerPacer, stats *requestStats) {
	local := newWorkerStats()
	defer stats.merge(&local)
	drainBuf := make([]byte, 32*1024)
	lastFlush := time.Now()

	for {
		if launchCtx.Err() != nil {
			return
		}
		if pacer != nil {
			ok, err := pacer.Wait(launchCtx)
			if err != nil || !ok {
				return
			}
		}
		if launchCtx.Err() != nil {
			return
		}

		start := time.Now()
		req, err := requestFactory.NewBackgroundRequest()
		if err != nil {
			if launchCtx.Err() != nil {
				return
			}
			local.recordError(time.Since(start))
			if shouldFlushWorkerStats(local.updates, lastFlush) {
				stats.merge(&local)
				lastFlush = time.Now()
			}
			continue
		}
		local.started++

		resp, err := client.Do(req)
		latency := time.Since(start)
		if err != nil {
			if launchCtx.Err() != nil {
				return
			}
			local.recordError(latency)
			if shouldFlushWorkerStats(local.updates, lastFlush) {
				stats.merge(&local)
				lastFlush = time.Now()
			}
			continue
		}

		n, copyErr := io.CopyBuffer(io.Discard, resp.Body, drainBuf)
		closeErr := resp.Body.Close()
		if copyErr != nil || closeErr != nil {
			local.recordError(latency)
		} else {
			local.recordResponse(resp.StatusCode, latency, n)
		}
		if shouldFlushWorkerStats(local.updates, lastFlush) {
			stats.merge(&local)
			lastFlush = time.Now()
		}
	}
}

func newRequestStats() *requestStats {
	return &requestStats{minLatency: math.MaxUint64}
}

func newWorkerStats() workerStats {
	return workerStats{
		minLatency: math.MaxUint64,
		dirtyCodes: make([]int, 0, 8),
	}
}

func (s *requestStats) merge(local *workerStats) {
	if local.updates == 0 && local.started == 0 && local.completed == 0 && local.errors == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.started += local.started
	s.completed += local.completed
	s.errors += local.errors
	s.bytesRead += local.bytesRead
	s.latencyTotal += local.latencyTotal
	s.latencyCount += local.latencyCount

	if local.minLatency < s.minLatency {
		s.minLatency = local.minLatency
	}
	if local.maxLatency > s.maxLatency {
		s.maxLatency = local.maxLatency
	}
	for _, code := range local.dirtyCodes {
		count := local.statusCodes[code]
		if count == 0 {
			continue
		}
		s.statusCodes[code] += count
	}

	*local = newWorkerStats()
}

func (s *requestStats) snapshot(elapsed time.Duration) StatsSnapshot {
	s.mu.RLock()
	started := s.started
	completed := s.completed
	errors := s.errors
	bytesRead := s.bytesRead
	latencyTotal := s.latencyTotal
	latencyCount := s.latencyCount
	minLatency := s.minLatency
	maxLatency := s.maxLatency
	statusArray := s.statusCodes
	s.mu.RUnlock()

	if minLatency == math.MaxUint64 {
		minLatency = 0
	}

	statusCodes := make(map[int]uint64)
	statusClasses := make(map[int]uint64)
	for code := 100; code < len(statusArray); code++ {
		count := statusArray[code]
		if count == 0 {
			continue
		}
		statusCodes[code] = count
		statusClasses[code/100] += count
	}

	snapshot := StatsSnapshot{
		Started:       started,
		Completed:     completed,
		Errors:        errors,
		BytesRead:     bytesRead,
		LatencyCount:  latencyCount,
		MinLatency:    time.Duration(minLatency),
		MaxLatency:    time.Duration(maxLatency),
		Elapsed:       elapsed,
		StatusCodes:   statusCodes,
		StatusClasses: statusClasses,
	}
	if latencyCount > 0 {
		snapshot.AvgLatency = time.Duration(latencyTotal / latencyCount)
	}
	return snapshot
}

func (s *workerStats) recordResponse(code int, latency time.Duration, bytesRead int64) {
	s.completed++
	if bytesRead > 0 {
		s.bytesRead += uint64(bytesRead)
	}
	s.recordLatency(latency)
	if code >= 0 && code < len(s.statusCodes) {
		if s.statusCodes[code] == 0 {
			s.dirtyCodes = append(s.dirtyCodes, code)
		}
		s.statusCodes[code]++
	}
	s.updates++
}

func (s *workerStats) recordError(latency time.Duration) {
	s.errors++
	s.recordLatency(latency)
	s.updates++
}

func (s *workerStats) recordLatency(latency time.Duration) {
	ns := uint64(latency)
	s.latencyTotal += ns
	s.latencyCount++
	if ns < s.minLatency {
		s.minLatency = ns
	}
	if ns > s.maxLatency {
		s.maxLatency = ns
	}
}

func splitRPS(total, workers, workerIndex int) int {
	if total <= 0 {
		return 0
	}
	base := total / workers
	if workerIndex < total%workers {
		base++
	}
	return base
}

func newWorkerPacer(rps int) *workerPacer {
	if rps <= 0 {
		return nil
	}

	interval := time.Second / time.Duration(rps)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	return &workerPacer{
		interval: interval,
		next:     time.Now(),
	}
}

func (p *workerPacer) Wait(ctx context.Context) (bool, error) {
	wait := time.Until(p.next)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	now := time.Now()
	if now.After(p.next) {
		p.next = now
	}
	p.next = p.next.Add(p.interval)
	return true, nil
}

func minNonZero(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func shouldFlushWorkerStats(updates int, lastFlush time.Time) bool {
	return updates >= workerStatsFlushEvery || time.Since(lastFlush) >= workerStatsFlushInterval
}
