// Package setup creates or updates, in every configured Sonarr and Radarr, the Webhook connection
// that sends On Import events to heraldarr.
//
// It speaks the *arr v3 notification API: find the connection by name, or start from the
// Webhook template in GET /notification/schema; set the URL, POST, the server.auth credentials
// and On Import as the only trigger; save it. Creating without forceSave makes the *arr send its
// Test event to heraldarr first and refuse the save if that fails; an update skips that when
// nothing changed, so then it asks for the Test event (POST /notification/test) first. Either
// way a success proves the URL and the credentials work.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/config"
	"github.com/wtwerner/heraldarr/internal/source/arr"
)

// DefaultName is the connection's name in the *arrs.
const DefaultName = "heraldarr"

// timeout bounds one request. A save waits for the *arr's Test event to reach heraldarr.
const timeout = 2 * time.Minute

// maxBody caps a response; a notification list is a few KB.
const maxBody = 4 << 20

// masked is how the *arrs show a stored password in their API responses.
const masked = "********"

// Options adjust Run.
type Options struct {
	Name      string // the connection to create or update; "" means DefaultName
	DryRun    bool   // report what would change and change nothing
	UserAgent string
}

// want is the connection one source should have.
type want struct {
	name, url, username, password string
}

// Run sets up every source in cfg, writing one report per source to w: what was saved and that
// the Test event was accepted, what would change (DryRun), or the *arr's error. A failing source
// doesn't stop the others; the returned error names every source that failed.
func Run(ctx context.Context, cfg *config.Config, opt Options, w io.Writer) error {
	if cfg.Server.PublicURL == "" {
		return errors.New("setup: server.public_url is required: the URL the *arrs use to reach heraldarr")
	}
	if opt.Name == "" {
		opt.Name = DefaultName
	}
	client := &http.Client{
		Timeout: timeout,
		// Go forwards custom headers such as X-Api-Key to wherever a redirect points: don't follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var user, pass string
	if a := cfg.Server.Auth; a != nil {
		user, pass = a.Username, a.Password.Value()
	}
	var failed []string
	for _, s := range cfg.Sources {
		wt := want{
			name: opt.Name, username: user, password: pass,
			url: strings.TrimRight(cfg.Server.PublicURL, "/") + "/hook/" + s.Name,
		}
		report, err := setupSource(ctx, client, opt, s, wt)
		if err != nil {
			report = "error: " + err.Error()
			failed = append(failed, s.Name)
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", s.Name, report); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("setup failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func setupSource(ctx context.Context, client *http.Client, opt Options, s config.Source, wt want) (string, error) {
	key := s.APIKey.Value()
	if s.ConfigXML != "" {
		var err error
		if key, err = arr.APIKeyFromConfigXML(s.ConfigXML); err != nil {
			return "", err
		}
	}
	api := &api{base: strings.TrimRight(s.URL, "/") + "/api/v3/", key: key, ua: opt.UserAgent, http: client}

	var all []map[string]any
	if err := api.do(ctx, http.MethodGet, "notification", nil, &all); err != nil {
		return "", err
	}
	var found []map[string]any
	for _, c := range all {
		if c["name"] == wt.name {
			found = append(found, c)
		}
	}
	var conn map[string]any
	switch len(found) {
	case 0:
		var schema []map[string]any
		if err := api.do(ctx, http.MethodGet, "notification/schema", nil, &schema); err != nil {
			return "", err
		}
		i := slices.IndexFunc(schema, func(c map[string]any) bool { return c["implementation"] == "Webhook" })
		if i < 0 {
			return "", errors.New("the *arr offers no Webhook connection")
		}
		conn = schema[i]
	case 1:
		conn = found[0]
		if impl := conn["implementation"]; impl != "Webhook" {
			return "", fmt.Errorf("the connection named %q is a %v connection, not a Webhook; rename it or pass -name", wt.name, impl)
		}
	default:
		return "", fmt.Errorf("%d connections are named %q; delete all but one", len(found), wt.name)
	}
	creating := len(found) == 0

	changes, hidden, err := apply(conn, wt)
	if err != nil {
		return "", err
	}
	if opt.DryRun {
		switch {
		case creating:
			return fmt.Sprintf("would create %q\n%s", wt.name, indent(summary(wt))), nil
		case len(changes) > 0:
			if hidden {
				changes = append(changes, "password: not compared (the *arr hides it)")
			}
			return fmt.Sprintf("would update %q\n%s", wt.name, indent(changes)), nil
		case hidden:
			return fmt.Sprintf("%q is up to date (the *arr hides the password, so it wasn't compared)", wt.name), nil
		}
		return fmt.Sprintf("%q is up to date", wt.name), nil
	}

	if creating {
		if err := api.do(ctx, http.MethodPost, "notification", conn, nil); err != nil {
			return "", fmt.Errorf("creating %q: %w", wt.name, err)
		}
		return fmt.Sprintf("created %q (On Import → %s), test event accepted", wt.name, wt.url), nil
	}
	id, ok := conn["id"].(json.Number)
	if !ok {
		return "", fmt.Errorf("connection %q has no id", wt.name)
	}
	// A save that changes nothing sends no Test event, so ask for one: a re-run is still the
	// end-to-end check. (A masked password that did change makes heraldarr get two; harmless.)
	if len(changes) == 0 {
		if err := api.do(ctx, http.MethodPost, "notification/test", conn, nil); err != nil {
			return "", fmt.Errorf("testing %q: %w", wt.name, err)
		}
	}
	if err := api.do(ctx, http.MethodPut, "notification/"+id.String(), conn, nil); err != nil {
		return "", fmt.Errorf("updating %q: %w", wt.name, err)
	}
	what := "no changes"
	if len(changes) > 0 {
		names := make([]string, len(changes))
		for i, c := range changes {
			names[i], _, _ = strings.Cut(c, ":")
		}
		what = strings.Join(names, ", ")
	}
	return fmt.Sprintf("updated %q (%s), test event accepted", wt.name, what), nil
}

// apply makes conn the connection wt describes, keeping everything else (the id, headers), and
// returns what changed, one "what: old → new" line each, without secrets. hidden reports that
// the *arr masked the stored password, so whether it changes is unknown.
func apply(conn map[string]any, wt want) (changes []string, hidden bool, err error) {
	if conn["name"] != wt.name {
		changes = append(changes, fmt.Sprintf("name: %v → %s", conn["name"], wt.name))
		conn["name"] = wt.name
	}

	// On Import only: every other trigger off, upgrades included.
	if _, ok := conn["onDownload"].(bool); !ok {
		return nil, false, errors.New("the Webhook connection has no On Import (onDownload) trigger")
	}
	triggers := []string{}
	for k, v := range conn {
		if _, ok := v.(bool); ok && strings.HasPrefix(k, "on") {
			triggers = append(triggers, k)
		}
	}
	slices.Sort(triggers)
	for _, k := range triggers {
		if on := k == "onDownload"; conn[k] != on {
			changes = append(changes, fmt.Sprintf("%s: %v → %v", k, conn[k], on))
			conn[k] = on
		}
	}
	if tags, _ := conn["tags"].([]any); len(tags) > 0 || conn["tags"] == nil {
		changes = append(changes, "tags: "+show(conn["tags"])+" → []")
	}
	conn["tags"] = []any{}

	fields, _ := conn["fields"].([]any)
	set := func(name string, v any, secret bool) error {
		for _, f := range fields {
			f, ok := f.(map[string]any)
			if !ok || f["name"] != name {
				continue
			}
			old := f["value"]
			if old == nil {
				old = ""
			}
			if show(old) != show(v) {
				switch {
				case secret && old == masked:
					hidden = true
				case secret:
					changes = append(changes, name+": changed")
				case name == "method":
					changes = append(changes, fmt.Sprintf("method: %s → %s", method(old), method(v)))
				default:
					changes = append(changes, fmt.Sprintf("%s: %s → %s", name, text(old), text(v)))
				}
			}
			f["value"] = v
			return nil
		}
		return fmt.Errorf("the Webhook connection has no %q field", name)
	}
	for _, err := range []error{
		set("url", wt.url, false),
		set("method", 1, false),
		set("username", wt.username, false),
		set("password", wt.password, true),
	} {
		if err != nil {
			return nil, false, err
		}
	}
	return changes, hidden, nil
}

// summary describes a connection about to be created, without the password.
func summary(wt want) []string {
	user, pass := wt.username, "from server.auth"
	if wt.username == "" {
		user, pass = "(none)", "(none)"
	}
	return []string{
		"url: " + wt.url,
		"method: POST",
		"username: " + user,
		"password: " + pass,
		"triggers: On Import only",
	}
}

func indent(lines []string) string { return "  " + strings.Join(lines, "\n  ") }

func show(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// text shows a string field's value as it reads in the *arr's form.
func text(v any) string {
	if s, ok := v.(string); ok {
		if s == "" {
			return "(none)"
		}
		return s
	}
	return show(v)
}

// method names the Webhook method setting: 1 is POST, 2 is PUT.
func method(v any) string {
	switch show(v) {
	case "1":
		return "POST"
	case "2":
		return "PUT"
	}
	return show(v)
}

// api calls one *arr's v3 API.
type api struct {
	base, key, ua string
	http          *http.Client
}

// do sends body (if any) as JSON and decodes a 2xx response into out (if any). A 4xx/5xx becomes
// an error with the *arr's validation messages; the body is never echoed, since a validation
// failure's attemptedValue can be the password that was sent.
func (a *api) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("X-Api-Key", a.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.ua != "" {
		req.Header.Set("User-Agent", a.ua)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err // *url.Error: method, URL (the key is a header) and cause
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s%s", method, path, resp.Status, validation(raw))
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // ids and settings go back exactly as they came
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

// validation formats the *arr's validation failures as ": Url: Unable to send test message".
func validation(raw []byte) string {
	var failures []struct {
		PropertyName string `json:"propertyName"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(raw, &failures) != nil {
		return ""
	}
	var msgs []string
	for _, f := range failures {
		m := f.ErrorMessage
		if f.PropertyName != "" {
			m = f.PropertyName + ": " + m
		}
		if m != "" {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return ""
	}
	return ": " + strings.Join(msgs, "; ")
}
