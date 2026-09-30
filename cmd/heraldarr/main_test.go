package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.String("to", "", "")
	fs.Bool("dry-run", false, "")
	for _, tc := range []struct{ in, want []string }{
		{[]string{"-config", "c.yaml", "serve"}, []string{"-config", "c.yaml", "serve"}},
		{[]string{"serve", "-config", "c.yaml"}, []string{"-config", "c.yaml", "serve"}},
		{[]string{"preview", "radarr", "5", "-to", "private"}, []string{"-to", "private", "preview", "radarr", "5"}},
		{[]string{"preview", "sonarr", "1", "S02", "-config=c.yaml"}, []string{"-config=c.yaml", "preview", "sonarr", "1", "S02"}},
		{[]string{"-dry-run", "setup"}, []string{"-dry-run", "setup"}},
		{[]string{"--dry-run", "setup", "-config", "c.yaml"}, []string{"--dry-run", "-config", "c.yaml", "setup"}},
	} {
		if got := interspersed(fs, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("interspersed(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSetupCommand runs the real command line against a fake *arr: the flags reach the setup
// package, wherever they are written.
func TestSetupCommand(t *testing.T) {
	var mu sync.Mutex
	var saved []map[string]any
	arr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification/schema":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"implementation": "Webhook", "onDownload": false, "tags": []any{},
				"fields": []any{
					map[string]any{"name": "url"},
					map[string]any{"name": "method"},
					map[string]any{"name": "username"},
					map[string]any{"name": "password"},
				},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/notification":
			var c map[string]any
			_ = json.NewDecoder(r.Body).Decode(&c)
			saved = append(saved, c)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(c)
		default:
			http.NotFound(w, r)
		}
	}))
	defer arr.Close()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(`
server: {public_url: "http://heraldarr.example.org:8790"}
sources:
  - {name: sonarr, kind: sonarr, url: "`+arr.URL+`", api_key: k}
destinations:
  - {name: tv, webhook_url: "https://discord.example.org/api/webhooks/1/x", public: true}
routes:
  - {sources: [sonarr], destination: tv}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"-dry-run", "setup", "-config", cfg}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(saved) != 0 {
		t.Errorf("-dry-run saved %v", saved)
	}
	mu.Unlock()
	if err := run([]string{"setup", "-config", cfg, "-name", "herald-test"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(saved) != 1 || saved[0]["name"] != "herald-test" {
		t.Errorf("saved %v, want one connection named herald-test", saved)
	}
	mu.Unlock()
	if err := run([]string{"setup", "extra", "-config", cfg}); err == nil {
		t.Error("setup with an argument: want an error")
	}
}

func TestLogFormat(t *testing.T) {
	for _, format := range []string{"", "text", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("HERALDARR_LOG_FORMAT", format)
			var buf bytes.Buffer
			log, err := newLogger(&buf)
			if err != nil {
				t.Fatal(err)
			}
			log.Info("posted", "title", "Lantern Season", "quiet", 5*time.Minute)
			var line map[string]any
			isJSON := json.Unmarshal(buf.Bytes(), &line) == nil
			switch {
			case format == "json" && (!isJSON || line["msg"] != "posted" || line["title"] != "Lantern Season" || line["quiet"] != "5m0s"):
				t.Errorf("want one JSON object, got %q", buf.String())
			case format != "json" && (isJSON || !strings.Contains(buf.String(), `msg=posted title="Lantern Season" quiet=5m0s`)):
				t.Errorf("want a text line, got %q", buf.String())
			}
		})
	}
	t.Setenv("HERALDARR_LOG_FORMAT", "yaml")
	if _, err := newLogger(&bytes.Buffer{}); err == nil {
		t.Error("an unknown format must be refused, not silently ignored")
	}
}

// Commands build their logger from the environment: a bad format stops them at startup.
func TestRunUsesLogFormat(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(`
sources:
  - {name: sonarr, kind: sonarr, url: "http://sonarr:8989", api_key: k}
destinations:
  - {name: tv, webhook_url: "https://discord.example.org/api/webhooks/1/x"}
routes:
  - {sources: [sonarr], destination: tv}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "-config", cfg}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	t.Setenv("HERALDARR_LOG_FORMAT", "yaml")
	if err := run([]string{"validate", "-config", cfg}); err == nil || !strings.Contains(err.Error(), "HERALDARR_LOG_FORMAT") {
		t.Errorf("validate with HERALDARR_LOG_FORMAT=yaml: %v, want the format refused", err)
	}
}
