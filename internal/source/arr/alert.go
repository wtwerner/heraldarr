package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Alert event types, as Sonarr, Radarr and Prowlarr send them.
const (
	EventHealth            = "Health"
	EventHealthRestored    = "HealthRestored"
	EventApplicationUpdate = "ApplicationUpdate"
)

// level is a health check level. The apps send it as a camelCase name ("warning"); the number of
// the enum (2) is read too.
type level string

func (l *level) UnmarshalJSON(b []byte) error {
	names := []string{"ok", domain.LevelNotice, domain.LevelWarning, domain.LevelError}
	if n, err := strconv.Atoi(string(b)); err == nil {
		if n < 0 || n >= len(names) {
			return fmt.Errorf("unknown health level %d", n)
		}
		*l = level(names[n])
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*l = level(strings.ToLower(s))
	return nil
}

// valid reports a level an issue can have ("ok" is a passing check, not an issue).
func (l level) valid() bool {
	return l == domain.LevelNotice || l == domain.LevelWarning || l == domain.LevelError
}

// ParseAlert decodes a Health, Health Restored or Application Update webhook (Sonarr, Radarr,
// Prowlarr). ok is false for every other event, which the caller handles or ignores.
func ParseAlert(source string, body []byte) (e domain.AlertEvent, ok bool, err error) {
	var w struct {
		EventType       string `json:"eventType"`
		InstanceName    string `json:"instanceName"`
		ApplicationURL  string `json:"applicationUrl"`
		Level           level  `json:"level"`
		Message         string `json:"message"`
		Type            string `json:"type"`
		WikiURL         string `json:"wikiUrl"`
		PreviousVersion string `json:"previousVersion"`
		NewVersion      string `json:"newVersion"`
	}
	var event struct {
		EventType string `json:"eventType"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return e, false, fmt.Errorf("bad json: %w", err)
	}
	var kind domain.AlertKind
	switch event.EventType {
	case EventHealth:
		kind = domain.AlertHealth
	case EventHealthRestored:
		kind = domain.AlertRestored
	case EventApplicationUpdate:
		kind = domain.AlertUpdate
	default:
		return e, false, nil
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return e, false, fmt.Errorf("%s: %w", event.EventType, err)
	}
	e = domain.AlertEvent{
		Source: source, Kind: kind, App: w.InstanceName, AppURL: w.ApplicationURL, Message: w.Message,
	}
	if kind == domain.AlertUpdate {
		if w.NewVersion == "" {
			return e, false, errors.New("ApplicationUpdate: no newVersion")
		}
		e.From, e.To = w.PreviousVersion, w.NewVersion
		return e, true, nil
	}
	if w.Type == "" {
		return e, false, fmt.Errorf("%s: no type (health check name)", event.EventType)
	}
	if !w.Level.valid() {
		return e, false, fmt.Errorf("%s: health level %q", event.EventType, w.Level)
	}
	e.Check, e.Level, e.WikiURL = w.Type, string(w.Level), w.WikiURL
	return e, true, nil
}

// DecodeHealth decodes GET /health: the checks failing now.
func DecodeHealth(body []byte) ([]domain.HealthCheck, error) {
	var rows []struct {
		Source  string `json:"source"` // the check's name
		Type    level  `json:"type"`   // its level
		Message string `json:"message"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&rows); err != nil {
		return nil, err
	}
	out := make([]domain.HealthCheck, 0, len(rows))
	for _, r := range rows {
		if r.Source == "" || !r.Type.valid() {
			continue
		}
		out = append(out, domain.HealthCheck{Check: r.Source, Level: string(r.Type), Message: r.Message})
	}
	return out, nil
}

var _ domain.HealthReader = (*Client)(nil)

// Health reads GET /api/v3/health (Prowlarr: /api/v1/health).
func (c *Client) Health(ctx context.Context) ([]domain.HealthCheck, error) {
	body, err := c.get(ctx, "health")
	if err != nil {
		return nil, err
	}
	h, err := DecodeHealth(body)
	if err != nil {
		return nil, fmt.Errorf("GET health: %w", err)
	}
	return h, nil
}
