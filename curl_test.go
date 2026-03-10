package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCurlCommand_MultilineJSON(t *testing.T) {
	t.Parallel()

	command := strings.Join([]string{
		"curl 'https://example.com/v1/items?existing=1' \\",
		"  -X POST \\",
		"  -H 'Content-Type: application/json' \\",
		"  -H 'X-Test: demo' \\",
		`  --data-binary '{"hello":"world"}'`,
	}, "\n")

	spec, err := ParseCurlCommand(command, t.TempDir())
	if err != nil {
		t.Fatalf("ParseCurlCommand returned error: %v", err)
	}

	if spec.Method != http.MethodPost {
		t.Fatalf("method = %s, want %s", spec.Method, http.MethodPost)
	}
	if spec.URL != "https://example.com/v1/items?existing=1" {
		t.Fatalf("url = %s", spec.URL)
	}
	if got := string(spec.Body); got != `{"hello":"world"}` {
		t.Fatalf("body = %q", got)
	}
	if got := spec.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := spec.Headers.Get("X-Test"); got != "demo" {
		t.Fatalf("x-test = %q", got)
	}
}

func TestParseCurlCommand_DataFileAndAuth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.json")
	headerPath := filepath.Join(dir, "headers.txt")

	if err := os.WriteFile(payloadPath, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile payload: %v", err)
	}
	if err := os.WriteFile(headerPath, []byte("Host: api.example.com\nX-One: 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile header: %v", err)
	}

	command := "curl --url 'https://example.com/api' --json @payload.json -H @headers.txt -u 'alice:secret' -k"
	spec, err := ParseCurlCommand(command, dir)
	if err != nil {
		t.Fatalf("ParseCurlCommand returned error: %v", err)
	}

	if spec.Method != http.MethodPost {
		t.Fatalf("method = %s", spec.Method)
	}
	if spec.Host != "api.example.com" {
		t.Fatalf("host = %q", spec.Host)
	}
	if got := string(spec.Body); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
	if got := spec.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := spec.Headers.Get("Accept"); got != "application/json" {
		t.Fatalf("accept = %q", got)
	}
	if got := spec.Headers.Get("X-One"); got != "1" {
		t.Fatalf("x-one = %q", got)
	}
	if got := spec.Headers.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("authorization = %q", got)
	}
	if !spec.Insecure {
		t.Fatalf("expected insecure mode to be true")
	}

	req, err := spec.NewRequest(context.Background())
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	if req.Host != "api.example.com" {
		t.Fatalf("request host = %q", req.Host)
	}
}

func TestParseCurlCommand_GetQuery(t *testing.T) {
	t.Parallel()

	command := "curl --url 'https://example.com/search?sort=desc' -G --data 'q=load+test' --data 'page=1'"
	spec, err := ParseCurlCommand(command, t.TempDir())
	if err != nil {
		t.Fatalf("ParseCurlCommand returned error: %v", err)
	}

	if spec.Method != http.MethodGet {
		t.Fatalf("method = %s", spec.Method)
	}
	if spec.Body != nil {
		t.Fatalf("expected empty body, got %q", string(spec.Body))
	}
	if spec.URL != "https://example.com/search?sort=desc&q=load+test&page=1" {
		t.Fatalf("url = %s", spec.URL)
	}
}

func TestParseCurlCommand_UnsupportedForm(t *testing.T) {
	t.Parallel()

	_, err := ParseCurlCommand("curl https://example.com -F 'file=@demo.txt'", t.TempDir())
	if err == nil {
		t.Fatalf("expected an error for unsupported --form")
	}
}
