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

// Back-catalog runs are on by default, one per show, edited as episodes land; quiet mode needs
// none of the run timings.
func TestBacklogDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	want := Backlog{Mode: "lead", Scope: "series", Edit: true, Settle: 5 * time.Minute, Idle: 24 * time.Hour, MaxHold: 24 * time.Hour}
	if cfg.Backlog != want || cfg.Timing.FollowingEarly != 24*time.Hour {
		t.Errorf("backlog = %+v, following_early = %s", cfg.Backlog, cfg.Timing.FollowingEarly)
	}
	cfg, err = Parse([]byte(minimal + "backlog: {mode: complete, scope: season, edit: false, max_hold: 48h}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Backlog; b.Mode != "complete" || b.Scope != "season" || b.Edit || b.MaxHold != 48*time.Hour || b.Settle != 5*time.Minute {
		t.Errorf("overrides: %+v", b)
	}
	if _, err := Parse([]byte(minimal + "backlog: {mode: quiet, settle: 0s}\n")); err != nil {
		t.Errorf("quiet mode with no settle: %v", err)
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
		{"heartbeat not http", "sources:", "server: {heartbeat_url: ftp://example.org/ping/tok123}\nsources:", "server.heartbeat_url must be an http"},
		{"heartbeat no host", "sources:", "server: {heartbeat_url: \"https:///ping/tok123\"}\nsources:", "server.heartbeat_url must be an http"},
		{"heartbeat file missing", "sources:", "server: {heartbeat_url: {file: /nonexistent/hb}}\nsources:", "server.heartbeat_url: open"},
		{"bad backlog mode", "routes:", "backlog: {mode: often}\nroutes:", "mode must be lead, complete or quiet"},
		{"bad backlog scope", "routes:", "backlog: {scope: episode}\nroutes:", "scope must be series or season"},
		{"zero settle", "routes:", "backlog: {settle: 0s}\nroutes:", "settle, idle and max_hold must be positive"},
		{"negative following_early", "routes:", "timing: {following_early: -1h}\nroutes:", "following_early must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(minimal, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "tok123") {
				t.Errorf("error reveals the secret: %v", err)
			}
		})
	}
}

func TestPublicURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"http://heraldarr:8790", ""},
		{"https://example.org/heraldarr/", ""},
		{"ftp://example.org", "must start with http:// or https://"},
		{"heraldarr:8790", "must start with http:// or https://"},
		{"http://", "has no host"},
		{"http://:8790", "has no host"},
		{"http://user:pw@example.org", "must not contain a username or password"},
		{"http://example.org/?a=1", "query or fragment"},
		{"http://example.org/#x", "query or fragment"},
		{"HTTPS://example.org", ""},
		{"http://exa mple.org", "invalid character"},
		{"http://user:pw-in-url@exa mple.org", "not a valid URL"},
		{"user:pw-in-url@example.org", "must start with http:// or https://"},
	} {
		cfg, err := Parse([]byte("server: {public_url: \"" + tc.url + "\"}\n" + minimal))
		if err != nil && strings.Contains(err.Error(), "pw-in-url") {
			t.Errorf("%s: the error reveals the password: %v", tc.url, err)
		}
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.url, err)
		case tc.want == "" && cfg.Server.PublicURL != tc.url:
			t.Errorf("%s: PublicURL = %q", tc.url, cfg.Server.PublicURL)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), "server.public_url") || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want an error containing %q", tc.url, err, tc.want)
		}
	}
}

func TestHeartbeatURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb")
	writeFile(t, path, "https://hc-ping.example.org/0000-tok\n")
	cfg, err := Parse([]byte("server: {heartbeat_url: {file: " + path + "}}\n" + minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.HeartbeatURL.Value(); got != "https://hc-ping.example.org/0000-tok" {
		t.Errorf("heartbeat_url = %q", got)
	}
	if cfg, _ := Parse([]byte(minimal)); !cfg.Server.HeartbeatURL.IsZero() {
		t.Error("heartbeat_url is optional")
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
