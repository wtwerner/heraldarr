package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
sources:
  - {name: sonarr, kind: sonarr, url: http://sonarr:8989, api_key: k}
destinations:
  - {name: tv, webhook_url: "https://discord.com/api/webhooks/1/x", public: true}
routes:
  - {sources: [sonarr], destination: tv}
`

func TestParseMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timing.QuietBacklog != 30*time.Minute || cfg.Timing.DigestFrom != 4 || cfg.Server.Listen != ":8790" {
		t.Errorf("defaults not applied: %+v %+v", cfg.Timing, cfg.Server)
	}
	if r, ok := cfg.RouteFor("sonarr"); !ok || r.Destination != "tv" {
		t.Errorf("RouteFor(sonarr) = %+v, %v", r, ok)
	}
}

func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, f := range []string{"webhook_password", "plex_token", "sonarr_api_key", "radarr_api_key"} {
		writeFile(t, filepath.Join(dir, f), "x")
	}
	for _, f := range []string{"discord_tv", "discord_movies"} {
		writeFile(t, filepath.Join(dir, f), "https://discord.com/api/webhooks/1/x\n")
	}
	if _, err := Parse([]byte(strings.ReplaceAll(string(raw), "/config/secrets", dir))); err != nil {
		t.Fatal(err)
	}
}

func TestSecretSources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "key"), "  from-file\n")
	t.Setenv("HERALDARR_TEST_KEY", "from-env")
	for _, tc := range []struct{ yaml, want string }{
		{`api_key: inline`, "inline"},
		{`api_key: {file: ` + filepath.Join(dir, "key") + `}`, "from-file"},
		{`api_key: {env: HERALDARR_TEST_KEY}`, "from-env"},
	} {
		cfg, err := Parse([]byte(strings.Replace(minimal, "api_key: k", tc.yaml, 1)))
		if err != nil {
			t.Fatalf("%s: %v", tc.yaml, err)
		}
		if got := cfg.Sources[0].APIKey.Value(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.yaml, got, tc.want)
		}
		if s := cfg.Sources[0].APIKey.String(); strings.Contains(s, tc.want) && tc.want != "inline" {
			t.Errorf("String() leaks the value: %s", s)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	for _, tc := range []struct{ name, from, to, want string }{
		{"missing secret file", "api_key: k", "api_key: {file: /nonexistent/key}", "no such file"},
		{"empty env", "api_key: k", "api_key: {env: HERALDARR_UNSET_VAR}", "is empty"},
		{"bad kind", "kind: sonarr", "kind: lidarr", "kind must be sonarr or radarr"},
		{"unknown destination", "destination: tv}", "destination: nope}", `unknown destination "nope"`},
		{"unrouted source", "routes:\n  - {sources: [sonarr], destination: tv}", "routes: []", "not used by any route"},
		{"tech details on public", "destination: tv}", "destination: tv, style: {tech_details: true}}", "public destination"},
		{"http webhook", "https://discord.com", "http://discord.com", "must be an https://"},
		{"unknown field", "public: true}", "public: true, colour: red}", "not found"},
		{"bad name", "name: sonarr,", "name: Sonarr TV,", "lowercase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(minimal, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestColor(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(minimal, "public: true", "public: false", 1) +
		`  - {sources: [], destination: tv, style: {color: "#9B59B6"}}` + "\n"))
	if err == nil {
		t.Fatal("route with no sources should fail")
	}
	cfg, err := Parse([]byte(strings.Replace(minimal, "destination: tv}", `destination: tv, style: {color: "#9B59B6"}}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routes[0].Style.Color; got != 0x9B59B6 {
		t.Errorf("color = %#x", int(got))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
