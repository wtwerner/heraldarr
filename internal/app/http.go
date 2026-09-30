package app

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
)

// maxBody bounds a webhook body; *arr payloads are a few KB.
const maxBody = 4 << 20

// Handler serves:
//
//	POST /hook/{source}  *arr webhook (Basic auth when configured)
//	GET  /health         liveness and pending count (no auth: for Docker health checks)
//	GET  /pending        what is waiting (auth)
//	POST /flush          post everything pending now (auth)
//	GET  /metrics        Prometheus metrics (auth)
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{source}", a.auth(a.hook))
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /pending", a.auth(a.pending))
	mux.HandleFunc("GET /metrics", a.auth(a.metricsHandler))
	mux.HandleFunc("POST /flush", a.auth(func(w http.ResponseWriter, _ *http.Request) {
		a.Kick(true)
		reply(w, http.StatusAccepted, map[string]any{"ok": true})
	}))
	return mux
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	if a.Auth == nil {
		return next
	}
	user, pass := []byte(a.Auth.Username), []byte(a.Auth.Password)
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), user)&subtle.ConstantTimeCompare([]byte(p), pass) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="heraldarr"`)
			reply(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (a *App) hook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("source")
	src, ok := a.Sources[name]
	if !ok {
		reply(w, http.StatusNotFound, map[string]any{"error": "unknown source"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		a.metrics.imports.add(1, name, importInvalid)
		reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	case err != nil:
		a.metrics.imports.add(1, name, importInvalid)
		reply(w, http.StatusBadRequest, map[string]any{"error": "could not read body"})
		return
	}
	imp, ok, err := arr.ParseWebhook(name, src.Kind, body)
	switch {
	case err != nil:
		a.metrics.imports.add(1, name, importInvalid)
		a.Log.Warn("unexpected webhook payload", "source", name, "err", err)
		reply(w, http.StatusBadRequest, map[string]any{"error": "unexpected payload"})
		return
	case !ok:
		a.metrics.imports.add(1, name, importIgnored)
		a.Log.Debug("webhook event ignored", "source", name) // Test, Grab, Rename…
		reply(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	line, err := a.batcher.Add(r.Context(), imp)
	if err != nil {
		a.metrics.imports.add(1, name, importError)
		a.Log.Error("queueing import", "source", name, "err", err)
		reply(w, http.StatusInternalServerError, map[string]any{"error": "could not queue"})
		return
	}
	a.metrics.imports.add(1, name, importResult(imp, line))
	a.Log.Info("import: " + line)
	reply(w, http.StatusOK, map[string]any{"ok": true})
}

// importResult classifies an import Batcher.Add accepted, from its log line: "+N" when it
// queued items, "…: already announced" when every item was posted within the reannounce window.
func importResult(imp domain.Import, line string) string {
	switch {
	case imp.Upgrade:
		return importUpgrade
	case strings.HasSuffix(line, ": already announced"):
		return importAlreadyAnnounced
	default:
		return importQueued
	}
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	batches, err := a.Store.LoadBatches(r.Context())
	if err != nil {
		reply(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store unavailable"})
		return
	}
	pending, runs := 0, 0
	for _, b := range batches {
		if b.Len() > 0 {
			pending++
		}
		if b.Run != nil {
			runs++
		}
	}
	reply(w, http.StatusOK, map[string]any{"ok": true, "pending": pending, "runs": runs})
}

func (a *App) pending(w http.ResponseWriter, r *http.Request) {
	batches, err := a.Store.LoadBatches(r.Context())
	if err != nil {
		reply(w, http.StatusServiceUnavailable, map[string]any{"error": "store unavailable"})
		return
	}
	now := a.Clock.Now()
	out := make([]map[string]any, 0, len(batches))
	for _, b := range batches {
		entry := map[string]any{
			"batch": batchName(b), "source": b.Source, "items": b.Len(),
			"following": b.Following, "quiet_for_min": now.Sub(b.Last).Round(6 * time.Second).Minutes(),
			"media_checks": b.MediaChecks, "tries": b.Tries,
		}
		if b.Backlog {
			entry["backlog"] = true
			if b.Season != nil {
				entry["season"] = *b.Season
			}
			if b.Run != nil {
				entry["announced"] = len(b.Run.Episodes)
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["batch"].(string) < out[j]["batch"].(string) })
	reply(w, http.StatusOK, out)
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
