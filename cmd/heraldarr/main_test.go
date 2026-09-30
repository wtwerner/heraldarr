package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
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
