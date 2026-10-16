package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Alert accent colors.
const (
	colorWarning  = 0xF1C40F
	colorError    = 0xE74C3C
	colorNotice   = 0x95A5A6
	colorResolved = 0x2ECC71
	colorUpdate   = 0x5865F2
)

const maxAlertMessage = 1500

// markdown escapes what would format an app's message in Discord.
var markdown = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`, `>`, `\>`, `#`, `\#`)

// Alert encodes an alert for Discord: open (held long enough to post), resolved, or an
// application update. now stamps the embed. Alerts go to non-public destinations only.
func Alert(a *domain.Alert, now time.Time) []domain.Layout {
	name := a.App
	if name == "" {
		name = a.Source
	}
	app := markdown.Replace(name) // for text Discord renders as markdown; embed author names aren't
	check := markdown.Replace(a.Check)
	var label, head, body, foot string
	var color int
	var buttons []domain.Button
	message := markdown.Replace(truncate(a.Message, maxAlertMessage))
	switch {
	case a.Kind == domain.AlertUpdate:
		label, color = "⬆️ Updated", colorUpdate
		body = "**" + markdown.Replace(joinNonEmpty(" → ", a.From, a.To)) + "**"
		foot = message
	case !a.Resolved.IsZero():
		label, color = "✅ Resolved", colorResolved
		body = message
		foot = joinNonEmpty(" · ", "Lasted "+lasted(a.Resolved.Sub(a.First)), check)
	default:
		switch a.Level {
		case domain.LevelError:
			label, color = "🛑 Error", colorError
		case domain.LevelNotice:
			label, color = "ℹ️ Notice", colorNotice
		default:
			label, color = "⚠️ Warning", colorWarning
		}
		body = "**" + message + "**"
		foot = joinNonEmpty(" · ", fmt.Sprintf("Failing since <t:%d:R>", a.First.Unix()), check)
		if web(a.WikiURL) {
			buttons = append(buttons, domain.Button{Label: "Wiki", URL: a.WikiURL, Emoji: "📖"})
		}
		if web(a.AppURL) {
			buttons = append(buttons, domain.Button{Label: "Open " + name, URL: a.AppURL, Emoji: "🔗"})
		}
	}
	head = label + " · " + app

	card := domain.Card{Headline: head, Color: color, Lines: []string{body}, Footer: foot, Buttons: buttons}
	parts := []any{text("-# " + head + "\n" + body)}
	if foot != "" {
		parts = append(parts, text("-# "+foot))
	}
	if len(buttons) > 0 {
		parts = append(parts, buttonsRow(card))
	}
	v2 := map[string]any{
		"flags":      flagComponentsV2,
		"components": []any{map[string]any{"type": componentBox, "accent_color": color, "components": parts}},
	}
	e := map[string]any{
		"author":      map[string]any{"name": label + " · " + name},
		"description": truncate(joinNonEmpty("\n\n", body, foot), maxEmbedDesc),
		"color":       color,
		"timestamp":   now.UTC().Format("2006-01-02T15:04:05-07:00"),
	}
	components := []any{}
	if len(buttons) > 0 {
		components = append(components, buttonsRow(card))
	}
	return []domain.Layout{
		{Name: "v2", Body: v2},
		{Name: "embed", Body: map[string]any{"embeds": []any{e}, "components": components}},
	}
}

// web reports a URL a Discord link button accepts.
func web(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// lasted writes how long an issue lasted: "14 min", "2 h 14 min", "2 d 2 h".
func lasted(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case d < time.Minute:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	h := m / 60
	return fmt.Sprintf("%d d %d h", h/24, h%24)
}
