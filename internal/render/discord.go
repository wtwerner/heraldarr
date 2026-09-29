package render

import (
	"strings"
	"time"

	"github.com/wtwerner/heraldarr/internal/domain"
)

// Discord limits and wire constants.
const (
	flagComponentsV2  = 1 << 15
	maxButtons        = 5
	maxEmbedDesc      = 4000
	componentRow      = 1
	componentButton   = 2
	componentSection  = 9
	componentText     = 10
	componentThumb    = 11
	componentGallery  = 12
	componentDivider  = 14
	componentBox      = 17
	buttonStyleLink   = 5
	emptyText         = "\xe2\x80\x8b" // U+200B ZERO WIDTH SPACE: a V2 text component can't be empty
	embedFooterNoName = "Plex"
)

// Layouts encodes a card for Discord, in the order to try them: "v2" (Components V2), "embed"
// (classic embed + link buttons) and "embed, inline links". now stamps the embeds.
func Layouts(card domain.Card, now time.Time) []domain.Layout {
	return []domain.Layout{
		{Name: "v2", Body: v2(card)},
		{Name: "embed", Body: embed(card, now, true)},
		{Name: "embed, inline links", Body: embed(card, now, false)},
	}
}

func buttonsRow(card domain.Card) map[string]any {
	buttons := []any{}
	for _, b := range first(card.Buttons, maxButtons) {
		buttons = append(buttons, map[string]any{
			"type": componentButton, "style": buttonStyleLink, "label": b.Label, "url": b.URL,
			"emoji": map[string]any{"name": b.Emoji},
		})
	}
	return map[string]any{"type": componentRow, "components": buttons}
}

func text(content string) map[string]any {
	return map[string]any{"type": componentText, "content": content}
}

// v2 is the Components V2 card: one container, buttons inside it.
func v2(card domain.Card) map[string]any {
	head := "## " + card.Title
	if card.URL != "" {
		head = "-# " + card.Headline + "\n## [" + card.Title + "](" + card.URL + ")"
	}
	if card.Overview != "" {
		head += "\n" + card.Overview
	}
	var top any = text(head)
	if card.Poster != "" {
		top = map[string]any{
			"type": componentSection, "components": []any{text(head)},
			"accessory": map[string]any{"type": componentThumb, "media": map[string]any{"url": card.Poster}},
		}
	}
	lines := strings.Join(card.Lines, "\n")
	if lines == "" {
		lines = emptyText
	}
	parts := []any{top, map[string]any{"type": componentDivider, "divider": true, "spacing": 1}, text(lines)}
	if len(card.Facts) > 0 {
		values := make([]string, len(card.Facts))
		for i, f := range card.Facts {
			values[i] = f.Value
		}
		parts = append(parts, text(strings.Join(values, " · ")))
	}
	if len(card.Gallery) > 0 {
		items := make([]any, len(card.Gallery))
		for i, u := range card.Gallery {
			items[i] = map[string]any{"media": map[string]any{"url": u}}
		}
		parts = append(parts, map[string]any{"type": componentGallery, "items": items})
	}
	if card.Footer != "" {
		parts = append(parts, text("-# "+card.Footer))
	}
	if len(card.Buttons) > 0 {
		parts = append(parts, buttonsRow(card))
	}
	return map[string]any{
		"flags":      flagComponentsV2,
		"components": []any{map[string]any{"type": componentBox, "accent_color": card.Color, "components": parts}},
	}
}

// embed is the classic embed, used only if Discord refuses the V2 card. Without buttons, the links
// go inline at the end of the description.
func embed(card domain.Card, now time.Time, buttons bool) map[string]any {
	description := strings.Join(card.Lines, "\n")
	if card.Overview != "" {
		description = "> " + card.Overview + "\n\n" + description
	}
	if !buttons && len(card.Buttons) > 0 {
		links := make([]string, len(card.Buttons))
		for i, b := range card.Buttons {
			links[i] = "[" + b.Label + "](" + b.URL + ")"
		}
		description += "\n\n" + strings.Join(links, " · ")
	}
	fields := []any{}
	for _, f := range card.Facts {
		fields = append(fields, map[string]any{"name": f.Name, "value": f.Value, "inline": true})
	}
	footer := card.Footer
	if footer == "" {
		footer = embedFooterNoName
	}
	e := map[string]any{
		"author":      map[string]any{"name": card.Headline},
		"description": truncate(description, maxEmbedDesc),
		"color":       card.Color,
		"fields":      fields,
		"footer":      map[string]any{"text": footer},
		"timestamp":   now.UTC().Format("2006-01-02T15:04:05-07:00"),
	}
	if card.URL != "" {
		e["title"], e["url"] = card.Title, card.URL
	}
	if card.Poster != "" {
		e["thumbnail"] = map[string]any{"url": card.Poster}
	}
	if len(card.Gallery) > 0 {
		e["image"] = map[string]any{"url": card.Gallery[0]}
	}
	components := []any{}
	if buttons && len(card.Buttons) > 0 {
		components = append(components, buttonsRow(card))
	}
	return map[string]any{"embeds": []any{e}, "components": components}
}
