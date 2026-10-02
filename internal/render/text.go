package render

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/wtwerner/heraldarr/internal/domain"
)

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// epRanges compresses episode numbers: 1,2,3,7,8 -> "E01–E03, E07–E08".
func epRanges(nums []int) string {
	nums = slices.Sorted(slices.Values(nums))
	var out []string
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		r := fmt.Sprintf("E%02d", nums[i])
		if j > i {
			r += fmt.Sprintf("–E%02d", nums[j])
		}
		out = append(out, r)
		i = j + 1
	}
	return strings.Join(out, ", ")
}

func seasonName(n int) string {
	if n == 0 {
		return "Specials"
	}
	return fmt.Sprintf("Season %d", n)
}

var resolution = regexp.MustCompile(`(2160|1080|720|576|480)p`)

// qualitySources maps a substring of the *arr quality name (case-insensitive) to its label; the
// first match wins.
var qualitySources = [][2]string{{"Remux", "Remux"}, {"Bluray", "Blu-ray"}, {"WEB", "WEB"}, {"HDTV", "HDTV"}, {"DVD", "DVD"}}

// qualityLabel turns an *arr quality name into what people read: "WEBDL-2160p" -> "4K WEB". A name
// with neither a resolution nor a known source comes back unchanged.
func qualityLabel(q string) string {
	if q == "" {
		return ""
	}
	res := ""
	if m := resolution.FindStringSubmatch(q); m != nil {
		res = m[0]
		if m[1] == "2160" {
			res = "4K"
		}
	}
	src := ""
	for _, s := range qualitySources {
		if strings.Contains(strings.ToLower(q), strings.ToLower(s[0])) {
			src = s[1]
			break
		}
	}
	if l := joinNonEmpty(" ", res, src); l != "" {
		return l
	}
	return q
}

// usualQuality is the most common quality label among the episodes, so a mixed batch shows the
// usual one rather than every mix. A tie goes to the label that appears first in episode order.
func usualQuality(eps []domain.Episode) string {
	counts := map[string]int{}
	var order []string
	for _, e := range eps {
		if l := qualityLabel(e.Quality); l != "" {
			if counts[l] == 0 {
				order = append(order, l)
			}
			counts[l]++
		}
	}
	best := ""
	for _, l := range order {
		if counts[l] > counts[best] {
			best = l
		}
	}
	return best
}

// trim shortens text to n runes, cutting at a word boundary and adding "…".
func trim(text string, n int) string {
	text = strings.TrimSpace(text)
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	cut := string(r[:n-3])
	if i := strings.LastIndex(cut, " "); i >= 0 {
		cut = cut[:i]
	}
	return cut + "…"
}

// truncate cuts s to n runes.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

var endsInYear = regexp.MustCompile(`\(\d{4}\)$`)

func titleYear(title string, year int) string {
	if year == 0 || endsInYear.MatchString(title) {
		return title
	}
	return fmt.Sprintf("%s (%d)", title, year)
}

// runtime formats minutes: "1h 04m", "59m", "" for none.
func runtime(minutes int) string {
	switch {
	case minutes >= 60:
		return fmt.Sprintf("%dh %02dm", minutes/60, minutes%60)
	case minutes != 0:
		return fmt.Sprintf("%dm", minutes)
	}
	return ""
}

func movieScores(in MovieInput) []string {
	var arr domain.Scores
	if in.Detail != nil {
		arr = domain.Scores{IMDb: in.Detail.IMDbRating, RTCritic: in.Detail.RTRating}
	}
	return scores(arr, in.Scores)
}

// scores formats IMDb, the Tomatometer and the Popcornmeter: "★ 7.4 IMDb", "🍅 91%", "🍿 85%".
// The *arr's score wins where both have one; only the media server has the Popcornmeter.
func scores(arr, media domain.Scores) []string {
	var out []string
	if v := cmp.Or(arr.IMDb, media.IMDb); v != 0 {
		out = append(out, fmt.Sprintf("★ %.1f IMDb", v))
	}
	if v := cmp.Or(arr.RTCritic, media.RTCritic); v != 0 {
		out = append(out, "🍅 "+strconv.FormatFloat(v, 'f', -1, 64)+"%")
	}
	if v := cmp.Or(arr.RTAudience, media.RTAudience); v != 0 {
		out = append(out, "🍿 "+strconv.FormatFloat(v, 'f', -1, 64)+"%")
	}
	return out
}

// mediaLabel is the technical footer: "4K Remux · DV HDR10+ · TrueHD Atmos 7.1 · 58.3 GB · GROUP".
func mediaLabel(m domain.Movie) []string {
	hdr := strings.TrimSpace(strings.ReplaceAll(m.HDR, "HDR10Plus", "HDR10+"))
	channels := ""
	if m.AudioChannels != 0 {
		channels = fmt.Sprintf("%.1f", m.AudioChannels)
	}
	size := ""
	if m.Size != 0 {
		size = fmt.Sprintf("%.1f GB", float64(m.Size)/1e9)
	}
	var out []string
	for _, x := range []string{qualityLabel(m.Quality), hdr, joinNonEmpty(" ", m.AudioCodec, channels), size, m.ReleaseGroup} {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func imdbURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://www.imdb.com/title/" + id + "/"
}

// linkButtons is the card's link row; a button without a URL is left out.
func linkButtons(mediaURL, trailer, imdbID, rottenTomatoes string) []domain.Button {
	var out []domain.Button
	for _, b := range []domain.Button{
		{Label: "Open in Plex", URL: mediaURL, Emoji: "▶️"},
		{Label: "Trailer", URL: trailer, Emoji: "🎞️"},
		{Label: "IMDb", URL: imdbURL(imdbID), Emoji: "🎬"},
		{Label: "Rotten Tomatoes", URL: rottenTomatoes, Emoji: "🍅"},
	} {
		if b.URL != "" {
			out = append(out, b)
		}
	}
	return out
}

// capLines keeps at most n lines, the last one saying how many were left out.
func capLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return append(lines[:n-1:n-1], fmt.Sprintf("…and %d more", len(lines)-(n-1)))
}

func first[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
