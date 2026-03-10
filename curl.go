package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const supportedCurlFlags = "-X/--request, -H/--header, -d/--data, --data-raw, --data-binary, --data-ascii, --json, -u/--user, -I/--head, -k/--insecure, -L/--location, -A/--user-agent, -e/--referer, -b/--cookie, -G/--get, --url"

type RequestSpec struct {
	Method          string
	URL             string
	Host            string
	Headers         http.Header
	Body            []byte
	Insecure        bool
	FollowRedirects bool
	factory         *requestFactory
}

type requestFactory struct {
	base          *http.Request
	body          []byte
	contentLength int64
	getBody       func() (io.ReadCloser, error)
	bodyPool      sync.Pool
}

var noBodyFactory = func() (io.ReadCloser, error) { return http.NoBody, nil }
var backgroundContext = context.Background()

type dataMode int

const (
	dataModeUnset dataMode = iota
	dataModeForm
	dataModeRaw
)

type dataAccumulator struct {
	mode  dataMode
	parts [][]byte
}

func LoadRequestSpec(path string) (RequestSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RequestSpec{}, err
	}
	return ParseCurlCommand(string(data), filepath.Dir(path))
}

func ParseCurlCommand(command, baseDir string) (RequestSpec, error) {
	tokens, err := tokenizeCurl(command)
	if err != nil {
		return RequestSpec{}, err
	}
	if len(tokens) == 0 {
		return RequestSpec{}, fmt.Errorf("curl command is empty")
	}
	if tokens[0] == "$" && len(tokens) > 1 {
		tokens = tokens[1:]
	}
	base := filepath.Base(tokens[0])
	if base != "curl" && base != "curl.exe" {
		return RequestSpec{}, fmt.Errorf("command must start with curl")
	}

	spec := RequestSpec{
		Headers: make(http.Header),
	}
	dataParts := dataAccumulator{}
	explicitMethod := false
	forceQuery := false

	for i := 1; i < len(tokens); i++ {
		token := tokens[i]
		if token == "" {
			continue
		}

		switch {
		case token == "--":
			if i+1 >= len(tokens) {
				return RequestSpec{}, fmt.Errorf("'--' must be followed by a URL")
			}
			if spec.URL != "" {
				return RequestSpec{}, fmt.Errorf("multiple URLs are not supported")
			}
			spec.URL = tokens[i+1]
			i++

		case matchesOption(token, "-X", "--request"):
			value, next, err := readOptionValue(tokens, i, token, "-X", "--request")
			if err != nil {
				return RequestSpec{}, err
			}
			spec.Method = strings.ToUpper(strings.TrimSpace(value))
			explicitMethod = true
			i = next

		case matchesOption(token, "-H", "--header"):
			value, next, err := readOptionValue(tokens, i, token, "-H", "--header")
			if err != nil {
				return RequestSpec{}, err
			}
			if err := addHeaderValue(spec.Headers, value, baseDir); err != nil {
				return RequestSpec{}, err
			}
			i = next

		case matchesAnyOption(token, "-d", "--data", "--data-raw", "--data-ascii"):
			value, next, err := readMultiOptionValue(tokens, i, token, "-d", "--data", "--data-raw", "--data-ascii")
			if err != nil {
				return RequestSpec{}, err
			}
			body, err := resolveDataValue(value, baseDir, false)
			if err != nil {
				return RequestSpec{}, err
			}
			if err := dataParts.add(dataModeForm, body); err != nil {
				return RequestSpec{}, err
			}
			i = next

		case matchesOption(token, "", "--data-binary"):
			value, next, err := readOptionValue(tokens, i, token, "", "--data-binary")
			if err != nil {
				return RequestSpec{}, err
			}
			body, err := resolveDataValue(value, baseDir, true)
			if err != nil {
				return RequestSpec{}, err
			}
			if err := dataParts.add(dataModeRaw, body); err != nil {
				return RequestSpec{}, err
			}
			i = next

		case matchesOption(token, "", "--json"):
			value, next, err := readOptionValue(tokens, i, token, "", "--json")
			if err != nil {
				return RequestSpec{}, err
			}
			body, err := resolveDataValue(value, baseDir, true)
			if err != nil {
				return RequestSpec{}, err
			}
			if err := dataParts.add(dataModeRaw, body); err != nil {
				return RequestSpec{}, err
			}
			if spec.Headers.Get("Content-Type") == "" {
				spec.Headers.Set("Content-Type", "application/json")
			}
			if spec.Headers.Get("Accept") == "" {
				spec.Headers.Set("Accept", "application/json")
			}
			i = next

		case matchesOption(token, "-u", "--user"):
			value, next, err := readOptionValue(tokens, i, token, "-u", "--user")
			if err != nil {
				return RequestSpec{}, err
			}
			auth := base64.StdEncoding.EncodeToString([]byte(value))
			spec.Headers.Set("Authorization", "Basic "+auth)
			i = next

		case token == "-I" || token == "--head":
			spec.Method = http.MethodHead
			explicitMethod = true

		case token == "-k" || token == "--insecure":
			spec.Insecure = true

		case token == "-L" || token == "--location":
			spec.FollowRedirects = true

		case token == "--compressed":
			if spec.Headers.Get("Accept-Encoding") == "" {
				spec.Headers.Set("Accept-Encoding", "gzip, deflate, br")
			}

		case matchesOption(token, "-A", "--user-agent"):
			value, next, err := readOptionValue(tokens, i, token, "-A", "--user-agent")
			if err != nil {
				return RequestSpec{}, err
			}
			spec.Headers.Set("User-Agent", value)
			i = next

		case matchesOption(token, "-e", "--referer"):
			value, next, err := readOptionValue(tokens, i, token, "-e", "--referer")
			if err != nil {
				return RequestSpec{}, err
			}
			spec.Headers.Set("Referer", value)
			i = next

		case matchesOption(token, "-b", "--cookie"):
			value, next, err := readOptionValue(tokens, i, token, "-b", "--cookie")
			if err != nil {
				return RequestSpec{}, err
			}
			if strings.HasPrefix(value, "@") {
				return RequestSpec{}, fmt.Errorf("%s cookie jar files are not supported; use -H 'Cookie: ...' instead", token)
			}
			spec.Headers.Add("Cookie", value)
			i = next

		case token == "-G" || token == "--get":
			forceQuery = true

		case matchesOption(token, "", "--url"):
			value, next, err := readOptionValue(tokens, i, token, "", "--url")
			if err != nil {
				return RequestSpec{}, err
			}
			if spec.URL != "" {
				return RequestSpec{}, fmt.Errorf("multiple URLs are not supported")
			}
			spec.URL = value
			i = next

		case isIgnoredNoValueFlag(token):
		case isIgnoredValueFlag(token):
			_, next, err := readMultiOptionValue(tokens, i, token, "-o", "--output", "-m", "--max-time", "--connect-timeout", "-w", "--write-out")
			if err != nil {
				return RequestSpec{}, err
			}
			i = next

		case matchesOption(token, "-F", "--form"):
			return RequestSpec{}, fmt.Errorf("%s is not supported yet; use a curl command with explicit body data instead", token)

		default:
			if strings.HasPrefix(token, "-") {
				return RequestSpec{}, fmt.Errorf("unsupported curl flag %s; supported flags: %s", token, supportedCurlFlags)
			}
			if spec.URL != "" {
				return RequestSpec{}, fmt.Errorf("multiple URLs are not supported")
			}
			spec.URL = token
		}
	}

	if spec.URL == "" {
		return RequestSpec{}, fmt.Errorf("curl command does not include a URL")
	}

	if forceQuery {
		if len(dataParts.parts) == 0 {
			return RequestSpec{}, fmt.Errorf("-G/--get was provided without any data flags")
		}
		if dataParts.mode != dataModeForm {
			return RequestSpec{}, fmt.Errorf("-G/--get only supports form-style data flags")
		}
		urlWithQuery, err := appendQuery(spec.URL, dataParts.body())
		if err != nil {
			return RequestSpec{}, err
		}
		spec.URL = urlWithQuery
		dataParts = dataAccumulator{}
		if !explicitMethod {
			spec.Method = http.MethodGet
		}
	}

	if spec.Method == "" {
		if len(dataParts.parts) > 0 {
			spec.Method = http.MethodPost
		} else {
			spec.Method = http.MethodGet
		}
	}

	spec.Body = dataParts.body()
	if len(spec.Body) > 0 && spec.Headers.Get("Content-Type") == "" && dataParts.mode == dataModeForm {
		spec.Headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if host := spec.Headers.Get("Host"); host != "" {
		spec.Host = host
		spec.Headers.Del("Host")
	}
	spec.Headers.Del("Content-Length")

	parsedURL, err := url.Parse(spec.URL)
	if err != nil {
		return RequestSpec{}, fmt.Errorf("invalid URL %q: %w", spec.URL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return RequestSpec{}, fmt.Errorf("only http and https URLs are supported")
	}
	if parsedURL.Host == "" {
		return RequestSpec{}, fmt.Errorf("URL must include a host")
	}
	spec.factory, err = spec.newRequestFactoryWithParsedURL(parsedURL)
	if err != nil {
		return RequestSpec{}, err
	}

	return spec, nil
}

func (s RequestSpec) NewRequest(ctx context.Context) (*http.Request, error) {
	factory := s.factory
	if factory == nil {
		var err error
		factory, err = s.newRequestFactoryWithContext(ctx)
		if err != nil {
			return nil, err
		}
	}
	if ctx == nil {
		return factory.NewBackgroundRequest()
	}
	return factory.NewRequest(ctx)
}

func (s RequestSpec) newRequestFactory() (*requestFactory, error) {
	return s.newRequestFactoryWithContext(context.Background())
}

func newRequestFactory(spec RequestSpec) (*requestFactory, error) {
	return spec.newRequestFactory()
}

func (s RequestSpec) newRequestFactoryWithContext(ctx context.Context) (*requestFactory, error) {
	if s.factory != nil {
		return s.factory, nil
	}
	parsedURL, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
	return s.newRequestFactoryWithParsedURLAndContext(parsedURL, ctx)
}

func (s RequestSpec) newRequestFactoryWithParsedURL(parsedURL *url.URL) (*requestFactory, error) {
	return s.newRequestFactoryWithParsedURLAndContext(parsedURL, context.Background())
}

func (s RequestSpec) newRequestFactoryWithParsedURLAndContext(parsedURL *url.URL, ctx context.Context) (*requestFactory, error) {
	base := &http.Request{
		Method:        s.Method,
		URL:           cloneURL(parsedURL),
		Header:        cloneHeaderShallow(s.Headers),
		Host:          s.Host,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Body:          http.NoBody,
		GetBody:       noBodyFactory,
		ContentLength: 0,
	}
	if ctx != nil {
		base = base.WithContext(ctx)
	}

	factory := &requestFactory{
		base: base,
	}
	factory.body = append([]byte(nil), s.Body...)
	factory.contentLength = int64(len(factory.body))
	if len(factory.body) == 0 {
		factory.getBody = base.GetBody
		return factory, nil
	}
	base.ContentLength = factory.contentLength
	factory.bodyPool.New = func() any {
		return &pooledBody{
			reader: bytes.NewReader(nil),
			pool:   &factory.bodyPool,
		}
	}
	factory.getBody = func() (io.ReadCloser, error) {
		body := factory.bodyPool.Get().(*pooledBody)
		body.reader.Reset(factory.body)
		return body, nil
	}
	base.GetBody = factory.getBody
	return factory, nil
}

func (f *requestFactory) NewRequest(ctx context.Context) (*http.Request, error) {
	if ctx == nil || ctx == backgroundContext || ctx == context.Background() {
		return f.NewBackgroundRequest()
	}
	req, err := f.NewBackgroundRequest()
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

func (f *requestFactory) NewBackgroundRequest() (*http.Request, error) {
	req := new(http.Request)
	*req = *f.base
	body, err := f.getBody()
	if err != nil {
		return nil, err
	}
	req.Body = body
	req.GetBody = f.getBody
	req.ContentLength = f.contentLength
	return req, nil
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	v := *u
	return &v
}

type preparedRequest struct {
	factory *requestFactory
}

func newPreparedRequest(spec RequestSpec) (*preparedRequest, error) {
	factory, err := spec.newRequestFactory()
	if err != nil {
		return nil, err
	}
	return &preparedRequest{factory: factory}, nil
}

func newPreparedRequestWithContext(spec RequestSpec, ctx context.Context) (*preparedRequest, error) {
	factory, err := spec.newRequestFactoryWithContext(ctx)
	if err != nil {
		return nil, err
	}
	return &preparedRequest{factory: factory}, nil
}

func (p *preparedRequest) NewRequest() *http.Request {
	req, err := p.factory.NewBackgroundRequest()
	if err != nil {
		panic(err)
	}
	return req
}

func cloneHeaderShallow(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

type pooledBody struct {
	reader *bytes.Reader
	pool   *sync.Pool
}

func (b *pooledBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *pooledBody) Close() error {
	b.reader.Reset(nil)
	b.pool.Put(b)
	return nil
}

func (d *dataAccumulator) add(mode dataMode, part []byte) error {
	if d.mode == dataModeUnset {
		d.mode = mode
	} else if d.mode != mode {
		return fmt.Errorf("mixing form-style data with raw/binary/json data is not supported")
	}
	cloned := append([]byte(nil), part...)
	d.parts = append(d.parts, cloned)
	return nil
}

func (d dataAccumulator) body() []byte {
	if len(d.parts) == 0 {
		return nil
	}
	if d.mode == dataModeRaw {
		total := 0
		for _, part := range d.parts {
			total += len(part)
		}
		out := make([]byte, 0, total)
		for _, part := range d.parts {
			out = append(out, part...)
		}
		return out
	}

	total := len(d.parts) - 1
	for _, part := range d.parts {
		total += len(part)
	}
	out := make([]byte, 0, total)
	for i, part := range d.parts {
		if i > 0 {
			out = append(out, '&')
		}
		out = append(out, part...)
	}
	return out
}

func matchesOption(token, shortFlag, longFlag string) bool {
	return matchesAnyOption(token, shortFlag, longFlag)
}

func matchesAnyOption(token string, flags ...string) bool {
	for _, flag := range flags {
		if flag == "" {
			continue
		}
		if token == flag {
			return true
		}
		if strings.HasPrefix(flag, "--") {
			if strings.HasPrefix(token, flag+"=") {
				return true
			}
			continue
		}
		if strings.HasPrefix(token, flag) && len(token) > len(flag) {
			return true
		}
	}
	return false
}

func readOptionValue(tokens []string, index int, token, shortFlag, longFlag string) (string, int, error) {
	if shortFlag != "" && token != shortFlag && strings.HasPrefix(token, shortFlag) {
		return token[len(shortFlag):], index, nil
	}
	if longFlag != "" && strings.HasPrefix(token, longFlag+"=") {
		return token[len(longFlag)+1:], index, nil
	}
	return requireValue(tokens, index, token)
}

func readMultiOptionValue(tokens []string, index int, token string, flags ...string) (string, int, error) {
	for _, flag := range flags {
		if flag == "" {
			continue
		}
		if strings.HasPrefix(flag, "--") {
			if strings.HasPrefix(token, flag+"=") {
				return token[len(flag)+1:], index, nil
			}
			if token == flag {
				return requireValue(tokens, index, token)
			}
			continue
		}
		if token == flag {
			return requireValue(tokens, index, token)
		}
		if strings.HasPrefix(token, flag) && len(token) > len(flag) {
			return token[len(flag):], index, nil
		}
	}
	return requireValue(tokens, index, token)
}

func requireValue(tokens []string, index int, flagName string) (string, int, error) {
	if index+1 >= len(tokens) {
		return "", index, fmt.Errorf("%s requires a value", flagName)
	}
	return tokens[index+1], index + 1, nil
}

func addHeaderValue(headers http.Header, value string, baseDir string) error {
	if strings.HasPrefix(value, "@") {
		path := resolvePath(baseDir, strings.TrimPrefix(value, "@"))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read header file %s: %w", path, err)
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if err := addHeaderLine(headers, line); err != nil {
				return err
			}
		}
		return nil
	}
	return addHeaderLine(headers, value)
}

func addHeaderLine(headers http.Header, line string) error {
	name, value, found := strings.Cut(line, ":")
	if !found {
		if strings.HasSuffix(line, ";") {
			name = strings.TrimSuffix(line, ";")
			value = ""
		} else {
			return fmt.Errorf("invalid header %q", line)
		}
	}
	name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("invalid header %q", line)
	}
	if name == "Content-Length" {
		return nil
	}
	headers.Add(name, strings.TrimSpace(value))
	return nil
}

func resolveDataValue(value, baseDir string, allowFileReference bool) ([]byte, error) {
	if strings.HasPrefix(value, "@") {
		if !allowFileReference {
			return nil, fmt.Errorf("file-backed bodies are only supported with --data-binary or --json")
		}
		path := resolvePath(baseDir, strings.TrimPrefix(value, "@"))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read payload file %s: %w", path, err)
		}
		return data, nil
	}
	return []byte(value), nil
}

func resolvePath(baseDir, path string) string {
	if filepath.IsAbs(path) || baseDir == "" {
		return path
	}
	return filepath.Join(baseDir, path)
}

func appendQuery(rawURL string, query []byte) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	extra := string(query)
	switch {
	case parsed.RawQuery == "":
		parsed.RawQuery = extra
	case extra != "":
		parsed.RawQuery = parsed.RawQuery + "&" + extra
	}
	return parsed.String(), nil
}

func tokenizeCurl(input string) ([]string, error) {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.TrimSpace(input)

	const (
		stateBare = iota
		stateSingle
		stateDouble
	)

	var (
		state       = stateBare
		escaping    bool
		tokenActive bool
		buf         strings.Builder
		tokens      []string
	)

	flush := func() {
		if !tokenActive {
			return
		}
		tokens = append(tokens, buf.String())
		buf.Reset()
		tokenActive = false
	}

	for i := 0; i < len(input); i++ {
		ch := input[i]

		switch state {
		case stateBare:
			if escaping {
				escaping = false
				if ch == '\n' {
					continue
				}
				buf.WriteByte(ch)
				tokenActive = true
				continue
			}
			switch ch {
			case '\\':
				escaping = true
			case '\'':
				state = stateSingle
				tokenActive = true
			case '"':
				state = stateDouble
				tokenActive = true
			case ' ', '\t', '\n':
				flush()
			default:
				buf.WriteByte(ch)
				tokenActive = true
			}

		case stateSingle:
			if ch == '\'' {
				state = stateBare
				continue
			}
			buf.WriteByte(ch)
			tokenActive = true

		case stateDouble:
			if escaping {
				escaping = false
				if ch == '\n' {
					continue
				}
				buf.WriteByte(ch)
				tokenActive = true
				continue
			}
			switch ch {
			case '\\':
				escaping = true
				tokenActive = true
			case '"':
				state = stateBare
			default:
				buf.WriteByte(ch)
				tokenActive = true
			}
		}
	}

	if escaping {
		return nil, fmt.Errorf("curl command ends with an unfinished escape")
	}
	if state == stateSingle {
		return nil, fmt.Errorf("curl command has an unterminated single-quoted string")
	}
	if state == stateDouble {
		return nil, fmt.Errorf("curl command has an unterminated double-quoted string")
	}

	flush()
	return tokens, nil
}

func isIgnoredNoValueFlag(token string) bool {
	switch token {
	case "-s", "--silent", "-S", "--show-error", "-v", "--verbose", "-i", "--include",
		"--compressed", "--http1.1", "--http2", "--path-as-is", "--globoff", "--fail", "--fail-with-body":
		return true
	default:
		return false
	}
}

func isIgnoredValueFlag(token string) bool {
	return matchesAnyOption(token, "-o", "--output", "-m", "--max-time", "--connect-timeout", "-w", "--write-out")
}
