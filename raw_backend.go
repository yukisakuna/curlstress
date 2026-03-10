package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	loadBackendNetHTTP = "nethttp"
	loadBackendRawH1   = "rawh1"
)

type rawRequestPlan struct {
	address        string
	method         string
	requestBytes   []byte
	requestTimeout time.Duration
	dialTimeout    time.Duration
	tlsConfig      *tls.Config
	responseReq    *http.Request
}

type rawConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

type rawResponseMeta struct {
	statusCode    int
	contentLength int64
	chunked       bool
	closeConn     bool
	skipBody      bool
}

func detectLoadBackend(spec RequestSpec) string {
	plan, ok, err := prepareRawBackend(spec)
	if err != nil || !ok || plan == nil {
		return loadBackendNetHTTP
	}
	return loadBackendRawH1
}

func prepareRawBackend(spec RequestSpec) (*rawRequestPlan, bool, error) {
	if spec.FollowRedirects {
		return nil, false, nil
	}
	if spec.Headers.Get("Expect") != "" {
		return nil, false, nil
	}

	parsedURL, err := url.Parse(spec.URL)
	if err != nil {
		return nil, false, fmt.Errorf("parse request URL: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, false, nil
	}

	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: parsedURL})
	if err != nil {
		return nil, false, fmt.Errorf("resolve proxy settings: %w", err)
	}
	if proxyURL != nil {
		return nil, false, nil
	}

	plan, err := newRawRequestPlan(spec, parsedURL)
	if err != nil {
		return nil, false, err
	}
	return plan, true, nil
}

func newRawRequestPlan(spec RequestSpec, parsedURL *url.URL) (*rawRequestPlan, error) {
	address := parsedURL.Host
	if address == "" {
		return nil, fmt.Errorf("raw backend requires a host")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		switch parsedURL.Scheme {
		case "https":
			address = net.JoinHostPort(parsedURL.Hostname(), "443")
		default:
			address = net.JoinHostPort(parsedURL.Hostname(), "80")
		}
	}

	hostHeader := spec.Host
	if hostHeader == "" {
		hostHeader = parsedURL.Host
	}

	uri := parsedURL.RequestURI()
	if uri == "" {
		uri = "/"
	}

	var request bytes.Buffer
	request.Grow(256 + len(spec.Body))
	fmt.Fprintf(&request, "%s %s HTTP/1.1\r\n", spec.Method, uri)
	fmt.Fprintf(&request, "Host: %s\r\n", hostHeader)

	hasConnectionHeader := false
	for name, values := range spec.Headers {
		lower := strings.ToLower(name)
		switch lower {
		case "host", "content-length", "transfer-encoding":
			continue
		case "connection":
			hasConnectionHeader = true
		}

		for _, value := range values {
			request.WriteString(name)
			request.WriteString(": ")
			request.WriteString(value)
			request.WriteString("\r\n")
		}
	}

	if !hasConnectionHeader {
		request.WriteString("Connection: keep-alive\r\n")
	}
	if len(spec.Body) > 0 {
		fmt.Fprintf(&request, "Content-Length: %d\r\n", len(spec.Body))
	}
	request.WriteString("\r\n")
	request.Write(spec.Body)

	plan := &rawRequestPlan{
		address:      address,
		method:       spec.Method,
		requestBytes: append([]byte(nil), request.Bytes()...),
		responseReq:  &http.Request{Method: spec.Method},
		dialTimeout:  10 * time.Second,
	}

	if parsedURL.Scheme == "https" {
		plan.tlsConfig = &tls.Config{
			ServerName:         parsedURL.Hostname(),
			InsecureSkipVerify: spec.Insecure,
			ClientSessionCache: tls.NewLRUClientSessionCache(256),
			NextProtos:         []string{"http/1.1"},
		}
	}

	return plan, nil
}

func runRawLoadTestWithContext(ctx context.Context, plan *rawRequestPlan, cfg LoadTestConfig, progress func(StatsSnapshot)) (LoadTestResult, error) {
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
			runRawWorker(launchCtx, plan.withTimeout(cfg), p, stats)
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

func (p *rawRequestPlan) withTimeout(cfg LoadTestConfig) *rawRequestPlan {
	clone := *p
	clone.requestTimeout = effectiveRequestTimeout(cfg)
	dialTimeout := minNonZero(clone.requestTimeout, 10*time.Second)
	if cfg.HTTPTimeout > 0 && cfg.HTTPTimeout < dialTimeout {
		dialTimeout = cfg.HTTPTimeout
	}
	if dialTimeout > 0 {
		clone.dialTimeout = dialTimeout
	}
	if clone.tlsConfig != nil {
		clonedTLS := clone.tlsConfig.Clone()
		clonedTLS.ClientSessionCache = tls.NewLRUClientSessionCache(maxInt(cfg.Workers*2, 128))
		clone.tlsConfig = clonedTLS
	}
	return &clone
}

func runRawWorker(launchCtx context.Context, plan *rawRequestPlan, pacer *workerPacer, stats *requestStats) {
	local := newWorkerStats()
	defer stats.merge(&local)

	drainBuf := make([]byte, 32*1024)
	lastFlush := time.Now()
	var session *rawConn
	defer closeRawConn(&session)

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
		if session == nil {
			conn, err := plan.connect()
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
			session = conn
		}

		if plan.requestTimeout > 0 {
			_ = session.conn.SetDeadline(time.Now().Add(plan.requestTimeout))
		} else {
			_ = session.conn.SetDeadline(time.Time{})
		}

		local.started++
		if _, err := session.bw.Write(plan.requestBytes); err != nil {
			closeRawConn(&session)
			local.recordError(time.Since(start))
			if shouldFlushWorkerStats(local.updates, lastFlush) {
				stats.merge(&local)
				lastFlush = time.Now()
			}
			continue
		}
		if err := session.bw.Flush(); err != nil {
			closeRawConn(&session)
			local.recordError(time.Since(start))
			if shouldFlushWorkerStats(local.updates, lastFlush) {
				stats.merge(&local)
				lastFlush = time.Now()
			}
			continue
		}

		meta, err := readRawResponseMeta(session.br, plan.method)
		latency := time.Since(start)
		if err != nil {
			closeRawConn(&session)
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

		n, copyErr := drainRawResponseBody(session.br, meta, drainBuf)
		if meta.closeConn {
			closeRawConn(&session)
		}

		if copyErr != nil {
			closeRawConn(&session)
			local.recordError(latency)
		} else {
			local.recordResponse(meta.statusCode, latency, n)
		}
		if shouldFlushWorkerStats(local.updates, lastFlush) {
			stats.merge(&local)
			lastFlush = time.Now()
		}
	}
}

func readRawResponseMeta(br *bufio.Reader, method string) (rawResponseMeta, error) {
	for {
		line, err := readCRLFLine(br)
		if err != nil {
			return rawResponseMeta{}, err
		}
		if len(line) < len("HTTP/1.1 200") || !bytes.HasPrefix(line, []byte("HTTP/")) {
			return rawResponseMeta{}, fmt.Errorf("invalid HTTP status line %q", line)
		}

		statusCode, err := parseStatusCode(line)
		if err != nil {
			return rawResponseMeta{}, err
		}

		meta, err := readRawHeaders(br, method, statusCode)
		if err != nil {
			return rawResponseMeta{}, err
		}
		meta.statusCode = statusCode
		if statusCode >= 100 && statusCode < 200 && statusCode != http.StatusSwitchingProtocols {
			continue
		}
		return meta, nil
	}
}

func readRawHeaders(br *bufio.Reader, method string, statusCode int) (rawResponseMeta, error) {
	meta := rawResponseMeta{contentLength: -1}
	if method == http.MethodHead || statusCode == http.StatusNoContent || statusCode == http.StatusNotModified || (statusCode >= 100 && statusCode < 200) {
		meta.skipBody = true
		meta.contentLength = 0
	}

	for {
		line, err := readCRLFLine(br)
		if err != nil {
			return rawResponseMeta{}, err
		}
		if len(line) == 0 {
			return meta, nil
		}

		name, value, ok := cutHeaderLine(line)
		if !ok {
			return rawResponseMeta{}, fmt.Errorf("malformed header line %q", line)
		}

		switch {
		case asciiEqualFold(name, "Content-Length"):
			n, err := parsePositiveInt64(value)
			if err != nil {
				return rawResponseMeta{}, fmt.Errorf("invalid content-length %q", value)
			}
			meta.contentLength = n
		case asciiEqualFold(name, "Transfer-Encoding"):
			if headerValueContainsToken(value, "chunked") {
				meta.chunked = true
				meta.contentLength = -1
			}
		case asciiEqualFold(name, "Connection"):
			if headerValueContainsToken(value, "close") {
				meta.closeConn = true
			}
		}
	}
}

func drainRawResponseBody(br *bufio.Reader, meta rawResponseMeta, buf []byte) (int64, error) {
	if meta.skipBody {
		return 0, nil
	}
	if meta.chunked {
		return drainChunkedBody(br, buf)
	}
	if meta.contentLength >= 0 {
		return io.CopyBuffer(io.Discard, io.LimitReader(br, meta.contentLength), buf)
	}
	if meta.closeConn {
		return io.CopyBuffer(io.Discard, br, buf)
	}
	return 0, fmt.Errorf("response body length is unknown without connection close")
}

func drainChunkedBody(br *bufio.Reader, buf []byte) (int64, error) {
	var total int64
	for {
		line, err := readCRLFLine(br)
		if err != nil {
			return total, err
		}

		size, err := parseChunkSize(line)
		if err != nil {
			return total, err
		}
		if size == 0 {
			for {
				trailer, err := readCRLFLine(br)
				if err != nil {
					return total, err
				}
				if len(trailer) == 0 {
					return total, nil
				}
			}
		}

		n, err := io.CopyBuffer(io.Discard, io.LimitReader(br, size), buf)
		total += n
		if err != nil {
			return total, err
		}
		if n != size {
			return total, io.ErrUnexpectedEOF
		}
		if err := expectCRLF(br); err != nil {
			return total, err
		}
	}
}

func readCRLFLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("invalid CRLF line ending")
	}
	return line[:len(line)-2], nil
}

func expectCRLF(br *bufio.Reader) error {
	b1, err := br.ReadByte()
	if err != nil {
		return err
	}
	b2, err := br.ReadByte()
	if err != nil {
		return err
	}
	if b1 != '\r' || b2 != '\n' {
		return fmt.Errorf("invalid chunk terminator")
	}
	return nil
}

func cutHeaderLine(line []byte) ([]byte, []byte, bool) {
	idx := bytes.IndexByte(line, ':')
	if idx <= 0 {
		return nil, nil, false
	}
	name := trimASCII(line[:idx])
	value := trimASCII(line[idx+1:])
	if len(name) == 0 {
		return nil, nil, false
	}
	return name, value, true
}

func parseStatusCode(line []byte) (int, error) {
	firstSpace := bytes.IndexByte(line, ' ')
	if firstSpace < 0 || firstSpace+4 > len(line) {
		return 0, fmt.Errorf("invalid status line %q", line)
	}
	codeBytes := line[firstSpace+1 : firstSpace+4]
	if codeBytes[0] < '0' || codeBytes[0] > '9' || codeBytes[1] < '0' || codeBytes[1] > '9' || codeBytes[2] < '0' || codeBytes[2] > '9' {
		return 0, fmt.Errorf("invalid status code %q", codeBytes)
	}
	return int(codeBytes[0]-'0')*100 + int(codeBytes[1]-'0')*10 + int(codeBytes[2]-'0'), nil
}

func parsePositiveInt64(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("empty integer")
	}
	var n int64
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid integer")
		}
		n = n*10 + int64(ch-'0')
	}
	return n, nil
}

func parseChunkSize(line []byte) (int64, error) {
	if idx := bytes.IndexByte(line, ';'); idx >= 0 {
		line = line[:idx]
	}
	line = trimASCII(line)
	if len(line) == 0 {
		return 0, fmt.Errorf("empty chunk size")
	}
	var n int64
	for _, ch := range line {
		nibble := hexNibble(ch)
		if nibble < 0 {
			return 0, fmt.Errorf("invalid chunk size %q", line)
		}
		n = n*16 + int64(nibble)
	}
	return n, nil
}

func hexNibble(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	default:
		return -1
	}
}

func headerValueContainsToken(value []byte, token string) bool {
	for _, part := range bytes.Split(value, []byte{','}) {
		if strings.EqualFold(string(trimASCII(part)), token) {
			return true
		}
	}
	return false
}

func asciiEqualFold(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ai := a[i]
		bi := b[i]
		if ai >= 'A' && ai <= 'Z' {
			ai += 'a' - 'A'
		}
		if bi >= 'A' && bi <= 'Z' {
			bi += 'a' - 'A'
		}
		if ai != bi {
			return false
		}
	}
	return true
}

func trimASCII(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

func (p *rawRequestPlan) connect() (*rawConn, error) {
	dialer := &net.Dialer{
		Timeout:   p.dialTimeout,
		KeepAlive: 30 * time.Second,
	}

	conn, err := dialer.Dial("tcp", p.address)
	if err != nil {
		return nil, err
	}

	if p.tlsConfig != nil {
		tlsConn := tls.Client(conn, p.tlsConfig.Clone())
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	return &rawConn{
		conn: conn,
		br:   bufio.NewReaderSize(conn, 64*1024),
		bw:   bufio.NewWriterSize(conn, 64*1024),
	}, nil
}

func closeRawConn(conn **rawConn) {
	if *conn == nil {
		return
	}
	_ = (*conn).conn.Close()
	*conn = nil
}

func hasCloseDirective(headers http.Header) bool {
	for _, value := range headers.Values("Connection") {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "close") {
				return true
			}
		}
	}
	return false
}

func effectiveWorkers(workers, rps int) int {
	if workers < 1 {
		return 1
	}
	if rps > 0 && rps < workers {
		return rps
	}
	return workers
}

func effectiveRequestTimeout(cfg LoadTestConfig) time.Duration {
	if cfg.RequestTimeout > 0 {
		return cfg.RequestTimeout
	}
	if cfg.HTTPTimeout > 0 {
		return cfg.HTTPTimeout
	}
	return 0
}
