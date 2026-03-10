package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	appName               = "curlstress"
	defaultCurlFile       = "curl.txt"
	defaultDuration       = 30 * time.Second
	defaultWorkers        = 128
	defaultQueueSize      = 1024
	defaultRequestTimeout = 15 * time.Second
	defaultHTTPTimeout    = 30 * time.Second
	defaultProgressEvery  = time.Second
	maxWorkers            = 10000
	maxQueueSize          = 1000000
	maxRPS                = 1000000
)

type cliConfig struct {
	CurlFile         string
	Duration         time.Duration
	Workers          int
	RPS              int
	QueueSize        int
	RequestTimeout   time.Duration
	HTTPTimeout      time.Duration
	ProgressInterval time.Duration
}

func main() {
	cfg, err := parseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	spec, err := LoadRequestSpec(cfg.CurlFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load %s: %v\n", cfg.CurlFile, err)
		os.Exit(1)
	}

	fmt.Printf(
		"load test start: method=%s url=%s backend=%s workers=%d rps=%s duration=%s body=%dB headers=%d insecure=%v redirects=%v\n",
		spec.Method,
		spec.URL,
		detectLoadBackend(spec),
		cfg.Workers,
		formatRPS(cfg.RPS),
		cfg.Duration,
		len(spec.Body),
		headerValueCount(spec.Headers),
		spec.Insecure,
		spec.FollowRedirects,
	)

	var last StatsSnapshot
	dynamicProgress := cfg.ProgressInterval > 0 && isTerminal(os.Stdout)
	lastProgressWidth := 0
	progress := func(s StatsSnapshot) {
		inflight := int64(s.Started) - int64(s.Completed) - int64(s.Errors)
		line := fmt.Sprintf(
			"live %s started=%d (%d/s) done=%d (%d/s) inflight=%d err=%d (%d/s) avg=%s status=%s\n",
			formatDuration(s.Elapsed),
			s.Started,
			s.Started-last.Started,
			s.Completed,
			s.Completed-last.Completed,
			inflight,
			s.Errors,
			s.Errors-last.Errors,
			formatDuration(s.AvgLatency),
			formatStatusSummary(s.StatusCodes),
		)
		if dynamicProgress {
			line = strings.TrimSuffix(line, "\n")
			padding := ""
			if lastProgressWidth > len(line) {
				padding = strings.Repeat(" ", lastProgressWidth-len(line))
			}
			fmt.Printf("\r%s%s", line, padding)
			lastProgressWidth = len(line)
		} else {
			fmt.Print(line)
		}
		last = s
	}
	if cfg.ProgressInterval == 0 {
		progress = nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result, err := RunLoadTestWithContext(ctx, spec, LoadTestConfig{
		Duration:         cfg.Duration,
		Workers:          cfg.Workers,
		RPS:              cfg.RPS,
		QueueSize:        cfg.QueueSize,
		RequestTimeout:   cfg.RequestTimeout,
		HTTPTimeout:      cfg.HTTPTimeout,
		ProgressInterval: cfg.ProgressInterval,
	}, progress)
	if dynamicProgress && lastProgressWidth > 0 {
		fmt.Print("\n")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "load test failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("done")
	fmt.Printf(
		"started=%d completed=%d inflight=%d errors=%d bytes=%s avg=%s min=%s max=%s req/s=%.1f elapsed=%s\n",
		result.Started,
		result.Completed,
		int64(result.Started)-int64(result.Completed)-int64(result.Errors),
		result.Errors,
		humanBytes(result.BytesRead),
		formatDuration(result.AvgLatency),
		formatDuration(result.MinLatency),
		formatDuration(result.MaxLatency),
		result.CompletedPerSec,
		result.Elapsed.Truncate(time.Millisecond),
	)
	fmt.Printf("status_breakdown: %s\n", formatStatusSummary(result.StatusCodes))
}

func parseCLI(args []string) (cliConfig, error) {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := cliConfig{}
	fs.StringVar(&cfg.CurlFile, "curl-file", defaultCurlFile, "path to a text file containing one curl command")
	fs.DurationVar(&cfg.Duration, "duration", defaultDuration, "how long to keep dispatching work")
	fs.IntVar(&cfg.Workers, "workers", defaultWorkers, "number of concurrent workers")
	fs.IntVar(&cfg.RPS, "rps", 0, "requests per second (0 = unthrottled)")
	fs.IntVar(&cfg.QueueSize, "queue", defaultQueueSize, "legacy compatibility flag; ignored in direct-worker mode")
	fs.DurationVar(&cfg.RequestTimeout, "req-timeout", defaultRequestTimeout, "per request timeout (0 = disabled)")
	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", defaultHTTPTimeout, "shared HTTP client timeout (0 = disabled)")
	fs.DurationVar(&cfg.ProgressInterval, "progress", defaultProgressEvery, "progress print interval (0 = disabled)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags]\n\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return cliConfig{}, err
	}
	if fs.NArg() != 0 {
		return cliConfig{}, fmt.Errorf("positional arguments are not supported")
	}
	if cfg.Duration <= 0 {
		return cliConfig{}, fmt.Errorf("duration must be > 0")
	}
	if cfg.Workers < 1 || cfg.Workers > maxWorkers {
		return cliConfig{}, fmt.Errorf("workers must be 1..%d", maxWorkers)
	}
	if cfg.RPS < 0 || cfg.RPS > maxRPS {
		return cliConfig{}, fmt.Errorf("rps must be 0..%d", maxRPS)
	}
	if cfg.QueueSize < 0 || cfg.QueueSize > maxQueueSize {
		return cliConfig{}, fmt.Errorf("queue must be 0..%d", maxQueueSize)
	}
	if cfg.RequestTimeout < 0 {
		return cliConfig{}, fmt.Errorf("req-timeout must be >= 0")
	}
	if cfg.HTTPTimeout < 0 {
		return cliConfig{}, fmt.Errorf("http-timeout must be >= 0")
	}
	if cfg.ProgressInterval < 0 {
		return cliConfig{}, fmt.Errorf("progress must be >= 0")
	}

	return cfg, nil
}

func formatRPS(rps int) string {
	if rps == 0 {
		return "unthrottled"
	}
	return fmt.Sprintf("%d", rps)
}

func headerValueCount(headers http.Header) int {
	total := 0
	for _, values := range headers {
		total += len(values)
	}
	return total
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0ms"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fµs", float64(d)/float64(time.Microsecond))
	}
	if d < time.Second {
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
	return d.Round(time.Millisecond).String()
}

func formatStatusSummary(statuses map[int]uint64) string {
	if len(statuses) == 0 {
		return "none"
	}

	codes := make([]int, 0, len(statuses))
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%d=%d", code, statuses[code]))
	}
	return strings.Join(parts, ",")
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
