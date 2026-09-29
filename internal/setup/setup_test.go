package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wtwerner/heraldarr/internal/config"
)

const (
	apiKey   = "arr-api-key-0000"
	password = "hook-password-1111"
	public   = "http://heraldarr:8790"
)

// fakeArr is a Sonarr/Radarr notification API: GET /notification, GET /notification/schema,
// POST /notification and PUT /notification/{id}. A save runs the "test event": it fails when
// reject is set, the way the *arr reports a webhook it couldn't reach.
type fakeArr struct {
	*httptest.Server
	mu     sync.Mutex
	conns  []map[string]any
	nextID int
	reject bool
	writes []string // "POST /api/v3/notification?", "PUT /api/v3/notification/7?"
}

func newFakeArr(t *testing.T, conns ...map[string]any) *fakeArr {
	t.Helper()
	f := &fakeArr{conns: conns, nextID: 100}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeArr) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != apiKey {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification":
		reply(http.StatusOK, f.conns)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification/schema":
		reply(http.StatusOK, []any{discordSchema(), webhookSchema()})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/notification",
		r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v3/notification/"):
		f.writes = append(f.writes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		var conn map[string]any
		if err := json.NewDecoder(r.Body).Decode(&conn); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.reject && r.URL.Query().Get("forceSave") != "true" {
			// Servarr's shape; attemptedValue can hold what was sent.
			reply(http.StatusBadRequest, []any{map[string]any{
				"propertyName": "Url", "errorMessage": "Unable to send test message",
				"attemptedValue": password, "severity": "error",
			}})
			return
		}
		if r.Method == http.MethodPost {
			f.nextID++
			conn["id"] = f.nextID
			f.conns = append(f.conns, conn)
			reply(http.StatusCreated, conn)
			return
		}
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/notification/"))
		for i, c := range f.conns {
			if num(c["id"]) == id {
				conn["id"] = id
				f.conns[i] = conn
				reply(http.StatusAccepted, conn)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeArr) snapshot() (conns []map[string]any, writes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, f.writes
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return -1
}

func fields(on map[string]bool, extra map[string]any) map[string]any {
	c := map[string]any{
		"includeHealthWarnings": false, "supportsOnGrab": true, "supportsOnDownload": true,
		"supportsOnUpgrade": true, "tags": []any{}, "fields": []any{},
	}
	for k, v := range on {
		c[k] = v
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// webhookSchema is Sonarr v4's Webhook template. Trigger defaults vary by version; some are
// true here so a create has to turn them off.
func webhookSchema() map[string]any {
	return fields(map[string]bool{
		"onGrab": true, "onDownload": false, "onUpgrade": true, "onImportComplete": false,
		"onRename": false, "onSeriesAdd": false, "onSeriesDelete": false, "onEpisodeFileDelete": false,
		"onEpisodeFileDeleteForUpgrade": true, "onHealthIssue": false, "onHealthRestored": false,
		"onApplicationUpdate": false, "onManualInteractionRequired": false,
	}, map[string]any{
		"name": "", "implementation": "Webhook", "implementationName": "Webhook",
		"configContract": "WebhookSettings",
		"fields": []any{
			map[string]any{"order": 0, "name": "url", "label": "Webhook URL", "type": "url"},
			map[string]any{"order": 1, "name": "method", "label": "Method", "value": 1, "type": "select"},
			map[string]any{"order": 2, "name": "username", "label": "Username", "type": "textbox"},
			map[string]any{"order": 3, "name": "password", "label": "Password", "type": "password"},
			map[string]any{"order": 4, "name": "headers", "label": "Headers", "value": []any{}, "type": "keyValueList"},
		},
	})
}

func discordSchema() map[string]any {
	return fields(map[string]bool{"onGrab": false, "onDownload": false}, map[string]any{
		"name": "", "implementation": "Discord", "implementationName": "Discord",
		"configContract": "DiscordSettings",
		"fields":         []any{map[string]any{"name": "webHookUrl", "type": "url"}},
	})
}

// staleHook is a heraldarr connection from an earlier install: old URL, PUT, upgrades and grabs
// on, a tag, and a custom header that must survive.
func staleHook(id int, name string) map[string]any {
	c := webhookSchema()
	c["id"], c["name"], c["tags"] = id, name, []any{3}
	c["onDownload"], c["onUpgrade"], c["onGrab"], c["onImportComplete"] = true, true, true, true
	c["fields"] = []any{
		map[string]any{"name": "url", "value": "http://old-host:8790/hook/sonarr"},
		map[string]any{"name": "method", "value": 2},
		map[string]any{"name": "username", "value": "someone"},
		map[string]any{"name": "password", "value": "old-password"},
		map[string]any{"name": "headers", "value": []any{map[string]any{"key": "X-Keep", "value": "me"}}},
	}
	return c
}

func testConfig(sources ...config.Source) *config.Config {
	return &config.Config{
		Server: config.Server{
			PublicURL: public,
			Auth:      &config.BasicAuth{Username: "heraldarr", Password: config.Literal(password)},
		},
		Sources: sources,
	}
}

func source(name string, f *fakeArr) config.Source {
	return config.Source{Name: name, Kind: "sonarr", URL: f.URL, APIKey: config.Literal(apiKey)}
}

func run(t *testing.T, cfg *config.Config, opt Options) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), cfg, opt, &out)
	s := out.String()
	for _, secret := range []string{password, apiKey} {
		if strings.Contains(s, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
			t.Errorf("output reveals a secret %q:\n%s\nerr: %v", secret, s, err)
		}
	}
	return s, err
}

func field(t *testing.T, conn map[string]any, name string) any {
	t.Helper()
	for _, f := range conn["fields"].([]any) {
		if f := f.(map[string]any); f["name"] == name {
			return f["value"]
		}
	}
	t.Fatalf("connection has no field %q: %v", name, conn["fields"])
	return nil
}

func named(conns []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, c := range conns {
		if c["name"] == name {
			out = append(out, c)
		}
	}
	return out
}

// assertHook checks everything the issue asks of the saved connection.
func assertHook(t *testing.T, conn map[string]any, source string) {
	t.Helper()
	if conn["implementation"] != "Webhook" {
		t.Errorf("implementation = %v", conn["implementation"])
	}
	if got, want := field(t, conn, "url"), public+"/hook/"+source; got != want {
		t.Errorf("url = %v, want %s", got, want)
	}
	if got := field(t, conn, "method"); num(got) != 1 {
		t.Errorf("method = %v, want 1 (POST)", got)
	}
	if got := field(t, conn, "username"); got != "heraldarr" {
		t.Errorf("username = %v", got)
	}
	if got := field(t, conn, "password"); got != password {
		t.Errorf("password = %v", got)
	}
	for k, v := range conn {
		if b, ok := v.(bool); ok && strings.HasPrefix(k, "on") && b != (k == "onDownload") {
			t.Errorf("%s = %v; want On Import only", k, b)
		}
	}
	if conn["onDownload"] != true {
		t.Errorf("onDownload = %v", conn["onDownload"])
	}
	if tags, ok := conn["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %#v, want []", conn["tags"])
	}
}

func TestCreatesFromSchema(t *testing.T) {
	f := newFakeArr(t, discordSchemaConn(1))
	out, err := run(t, testConfig(source("sonarr", f)), Options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	conns, writes := f.snapshot()
	if len(writes) != 1 || writes[0] != "POST /api/v3/notification?" {
		t.Errorf("writes = %q, want one POST without forceSave", writes)
	}
	hooks := named(conns, "heraldarr")
	if len(hooks) != 1 {
		t.Fatalf("%d connections named heraldarr", len(hooks))
	}
	assertHook(t, hooks[0], "sonarr")
	if want := `sonarr: created "heraldarr"`; !strings.Contains(out, want) || !strings.Contains(out, "test event accepted") {
		t.Errorf("output %q, want %q and test event accepted", out, want)
	}
}

func discordSchemaConn(id int) map[string]any {
	c := discordSchema()
	c["id"], c["name"], c["onDownload"] = id, "friends", true
	return c
}

func TestUpdatesInPlace(t *testing.T) {
	f := newFakeArr(t, discordSchemaConn(1), staleHook(7, "heraldarr"))
	out, err := run(t, testConfig(source("sonarr", f)), Options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	conns, writes := f.snapshot()
	if len(writes) != 1 || writes[0] != "PUT /api/v3/notification/7?" {
		t.Errorf("writes = %q, want one PUT to 7 without forceSave", writes)
	}
	if len(conns) != 2 {
		t.Fatalf("%d connections, want 2 (no duplicate)", len(conns))
	}
	if conns[0]["name"] != "friends" || conns[0]["onDownload"] != true {
		t.Errorf("another connection changed: %v", conns[0])
	}
	assertHook(t, conns[1], "sonarr")
	if h := field(t, conns[1], "headers").([]any); len(h) != 1 {
		t.Errorf("headers = %v, want the existing one kept", h)
	}
	if !strings.Contains(out, `sonarr: updated "heraldarr"`) || !strings.Contains(out, "test event accepted") {
		t.Errorf("output = %q", out)
	}
}

func TestUpToDateStillSavesForTheTestEvent(t *testing.T) {
	f := newFakeArr(t)
	cfg := testConfig(source("sonarr", f))
	if _, err := run(t, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	conns, writes := f.snapshot()
	if len(conns) != 1 || len(writes) != 2 || !strings.HasPrefix(writes[1], "PUT ") {
		t.Errorf("conns %d, writes %q: want the second run to PUT the same connection", len(conns), writes)
	}
	if !strings.Contains(out, "no changes") || !strings.Contains(out, "test event accepted") {
		t.Errorf("output = %q", out)
	}
}

func TestName(t *testing.T) {
	f := newFakeArr(t, staleHook(7, "heraldarr"), staleHook(8, "herald-test"))
	if out, err := run(t, testConfig(source("sonarr", f)), Options{Name: "herald-test"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	conns, writes := f.snapshot()
	if len(writes) != 1 || writes[0] != "PUT /api/v3/notification/8?" {
		t.Errorf("writes = %q, want PUT to 8", writes)
	}
	if field(t, conns[0], "url") != "http://old-host:8790/hook/sonarr" {
		t.Error("the connection named heraldarr changed")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	stale := newFakeArr(t, staleHook(7, "heraldarr"))
	empty := newFakeArr(t)
	cfg := testConfig(source("sonarr", stale), source("radarr", empty))
	out, err := run(t, cfg, Options{DryRun: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []*fakeArr{stale, empty} {
		if _, writes := f.snapshot(); len(writes) != 0 {
			t.Errorf("dry run wrote %q", writes)
		}
	}
	for _, want := range []string{
		`sonarr: would update "heraldarr"`,
		"url: http://old-host:8790/hook/sonarr → " + public + "/hook/sonarr",
		"method: PUT → POST",
		"username: someone → heraldarr",
		"password: changed",
		"onGrab: true → false",
		"onUpgrade: true → false",
		"onImportComplete: true → false",
		"tags: [3] → []",
		`radarr: would create "heraldarr"`,
		"url: " + public + "/hook/radarr",
		"triggers: On Import only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "onDownload") {
		t.Errorf("reports an unchanged trigger:\n%s", out)
	}
}

func TestRejectedTestEvent(t *testing.T) {
	bad := newFakeArr(t)
	bad.reject = true
	good := newFakeArr(t)
	out, err := run(t, testConfig(source("sonarr", bad), source("radarr", good)), Options{})
	if err == nil || !strings.Contains(err.Error(), "sonarr") {
		t.Errorf("err = %v, want one naming sonarr", err)
	}
	if !strings.Contains(out, "sonarr: ") || !strings.Contains(out, "Url: Unable to send test message") {
		t.Errorf("output lacks the *arr's error:\n%s", out)
	}
	if strings.Contains(out, "sonarr: created") {
		t.Errorf("reports a rejected save as created:\n%s", out)
	}
	if conns, _ := good.snapshot(); len(conns) != 1 || !strings.Contains(out, `radarr: created "heraldarr"`) {
		t.Errorf("a failing source stopped the next one:\n%s", out)
	}
}

func TestOtherImplementationWithTheName(t *testing.T) {
	c := discordSchemaConn(1)
	c["name"] = "heraldarr"
	f := newFakeArr(t, c)
	out, err := run(t, testConfig(source("sonarr", f)), Options{})
	if err == nil || !strings.Contains(out, "Discord") {
		t.Errorf("err = %v, output %q: want a refusal naming the Discord connection", err, out)
	}
	if _, writes := f.snapshot(); len(writes) != 0 {
		t.Errorf("wrote %q", writes)
	}
}

func TestDuplicateNames(t *testing.T) {
	f := newFakeArr(t, staleHook(7, "heraldarr"), staleHook(8, "heraldarr"))
	out, err := run(t, testConfig(source("sonarr", f)), Options{})
	if err == nil || !strings.Contains(out, "2 connections") {
		t.Errorf("err = %v, output %q: want a refusal", err, out)
	}
	if _, writes := f.snapshot(); len(writes) != 0 {
		t.Errorf("wrote %q", writes)
	}
}

func TestPublicURLRequired(t *testing.T) {
	f := newFakeArr(t)
	cfg := testConfig(source("sonarr", f))
	cfg.Server.PublicURL = ""
	_, err := run(t, cfg, Options{})
	if err == nil || !strings.Contains(err.Error(), "server.public_url") {
		t.Errorf("err = %v", err)
	}
	if _, writes := f.snapshot(); len(writes) != 0 {
		t.Errorf("wrote %q", writes)
	}
}

func TestPublicURLWithPath(t *testing.T) {
	f := newFakeArr(t)
	cfg := testConfig(source("sonarr", f))
	cfg.Server.PublicURL = "https://example.org/heraldarr/"
	if _, err := run(t, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	conns, _ := f.snapshot()
	if got := field(t, conns[0], "url"); got != "https://example.org/heraldarr/hook/sonarr" {
		t.Errorf("url = %v", got)
	}
}

func TestNoAuth(t *testing.T) {
	f := newFakeArr(t, staleHook(7, "heraldarr"))
	cfg := testConfig(source("sonarr", f))
	cfg.Server.Auth = nil
	if _, err := run(t, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	conns, _ := f.snapshot()
	if u, p := field(t, conns[0], "username"), field(t, conns[0], "password"); u != "" || p != "" {
		t.Errorf("username %q, password %q: want both cleared", u, p)
	}
}

func TestConfigXMLKey(t *testing.T) {
	f := newFakeArr(t)
	path := filepath.Join(t.TempDir(), "config.xml")
	if err := os.WriteFile(path, fmt.Appendf(nil, "<Config><ApiKey>%s</ApiKey></Config>", apiKey), 0o600); err != nil {
		t.Fatal(err)
	}
	src := source("sonarr", f)
	src.APIKey, src.ConfigXML = config.Secret{}, path
	if out, err := run(t, testConfig(src), Options{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if conns, _ := f.snapshot(); len(conns) != 1 {
		t.Errorf("%d connections", len(conns))
	}
}
