package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

var alertT0 = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func heldAlert() *domain.Alert {
	return &domain.Alert{
		Key: "prowlarr:health:IndexerStatusCheck", Source: "prowlarr", Kind: domain.AlertHealth,
		App: "Prowlarr", AppURL: "https://prowlarr.example.org", Check: "IndexerStatusCheck", Level: domain.LevelWarning,
		Message: "Indexers unavailable due to failures: Example_One", WikiURL: "https://wiki.example.org/indexers",
		First: alertT0,
	}
}

func v2Text(t *testing.T, l domain.Layout) (string, int) {
	t.Helper()
	box := l.Body["components"].([]any)[0].(map[string]any)
	var texts []string
	for _, c := range box["components"].([]any) {
		if c := c.(map[string]any); c["type"] == componentText {
			texts = append(texts, c["content"].(string))
		}
	}
	return strings.Join(texts, "\n"), box["accent_color"].(int)
}

func buttonURLs(t *testing.T, body map[string]any) []string {
	t.Helper()
	b, _ := json.Marshal(body)
	var urls []string
	for _, part := range strings.Split(string(b), `"url":"`)[1:] {
		u, _, _ := strings.Cut(part, `"`)
		urls = append(urls, u)
	}
	return urls
}

func TestAlertOpen(t *testing.T) {
	ls := Alert(heldAlert(), alertT0.Add(40*time.Minute))
	if len(ls) != 2 || ls[0].Name != "v2" || ls[1].Name != "embed" {
		t.Fatalf("layouts %v", ls)
	}
	txt, color := v2Text(t, ls[0])
	for _, want := range []string{
		"⚠️ Warning · Prowlarr", `Indexers unavailable due to failures: Example\_One`,
		"Failing since <t:1781524800:R>", "IndexerStatusCheck",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("v2 text lacks %q:\n%s", want, txt)
		}
	}
	if color != colorWarning {
		t.Errorf("color %#x", color)
	}
	if got := buttonURLs(t, ls[0].Body); len(got) != 2 || got[0] != "https://wiki.example.org/indexers" || got[1] != "https://prowlarr.example.org" {
		t.Errorf("buttons %v", got)
	}
	e := ls[1].Body["embeds"].([]any)[0].(map[string]any)
	if e["author"].(map[string]any)["name"] != "⚠️ Warning · Prowlarr" || !strings.Contains(e["description"].(string), "Example\\_One") ||
		e["color"] != colorWarning {
		t.Errorf("embed %v", e)
	}
}

func TestAlertLevels(t *testing.T) {
	for level, want := range map[string]struct {
		head  string
		color int
	}{
		domain.LevelError:  {"🛑 Error · Prowlarr", colorError},
		domain.LevelNotice: {"ℹ️ Notice · Prowlarr", colorNotice},
	} {
		a := heldAlert()
		a.Level = level
		txt, color := v2Text(t, Alert(a, alertT0)[0])
		if !strings.Contains(txt, want.head) || color != want.color {
			t.Errorf("%s: color %#x text %s", level, color, txt)
		}
	}
}

func TestAlertResolved(t *testing.T) {
	a := heldAlert()
	a.Resolved = alertT0.Add(2*time.Hour + 14*time.Minute)
	ls := Alert(a, a.Resolved)
	txt, color := v2Text(t, ls[0])
	if color != colorResolved || !strings.Contains(txt, "✅ Resolved · Prowlarr") || !strings.Contains(txt, "Lasted 2 h 14 min") ||
		strings.Contains(txt, "Failing since") {
		t.Errorf("color %#x text %s", color, txt)
	}
	if got := buttonURLs(t, ls[0].Body); len(got) != 0 {
		t.Errorf("resolved card keeps buttons %v", got)
	}
}

func TestAlertUpdate(t *testing.T) {
	a := &domain.Alert{Kind: domain.AlertUpdate, Source: "sonarr", Message: "Sonarr updated", From: "4.0.1", To: "4.0.2", First: alertT0}
	txt, color := v2Text(t, Alert(a, alertT0)[0])
	if color != colorUpdate || !strings.Contains(txt, "⬆️ Updated · sonarr") || !strings.Contains(txt, "4.0.1 → 4.0.2") {
		t.Errorf("color %#x text %s", color, txt) // no instance name: the source's name
	}
}

func TestAlertDropsUnsafeLinks(t *testing.T) {
	a := heldAlert()
	a.AppURL, a.WikiURL = "prowlarr:9696", "javascript:alert(1)"
	if got := buttonURLs(t, Alert(a, alertT0)[0].Body); len(got) != 0 {
		t.Errorf("buttons %v: Discord refuses a message with a non-http button", got)
	}
}

func TestLasted(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:              "under a minute",
		14 * time.Minute:              "14 min",
		2*time.Hour + 14*time.Minute:  "2 h 14 min",
		3 * time.Hour:                 "3 h",
		50*time.Hour + 20*time.Minute: "2 d 2 h",
	} {
		if got := lasted(d); got != want {
			t.Errorf("lasted(%v) = %q, want %q", d, got, want)
		}
	}
}
