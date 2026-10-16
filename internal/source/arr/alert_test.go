package arr_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wtwerner/heraldarr/internal/domain"
	"github.com/wtwerner/heraldarr/internal/source/arr"
)

func TestParseAlert(t *testing.T) {
	cases := []struct {
		name string
		body string
		want domain.AlertEvent
		ok   bool
	}{
		{
			name: "health warning (Prowlarr)",
			body: `{"level":"warning","message":"Indexers unavailable due to failures: Example","type":"IndexerStatusCheck",
				"wikiUrl":"https://wiki.example.org/prowlarr/system#indexers","eventType":"Health","instanceName":"Prowlarr",
				"applicationUrl":"https://prowlarr.example.org"}`,
			ok: true,
			want: domain.AlertEvent{
				Source: "src", Kind: domain.AlertHealth, App: "Prowlarr", AppURL: "https://prowlarr.example.org",
				Check: "IndexerStatusCheck", Level: domain.LevelWarning, Message: "Indexers unavailable due to failures: Example",
				WikiURL: "https://wiki.example.org/prowlarr/system#indexers",
			},
		},
		{
			name: "health restored, level as a number (error)",
			body: `{"level":3,"message":"Unable to communicate with the download client","type":"DownloadClientCheck",
				"eventType":"HealthRestored","instanceName":"Sonarr"}`,
			ok: true,
			want: domain.AlertEvent{
				Source: "src", Kind: domain.AlertRestored, App: "Sonarr", Check: "DownloadClientCheck",
				Level: domain.LevelError, Message: "Unable to communicate with the download client",
			},
		},
		{
			name: "level in another case (notice)",
			body: `{"level":"Notice","message":"m","type":"UpdateCheck","eventType":"Health"}`,
			ok:   true,
			want: domain.AlertEvent{Source: "src", Kind: domain.AlertHealth, Check: "UpdateCheck", Level: domain.LevelNotice, Message: "m"},
		},
		{
			name: "application update",
			body: `{"message":"Radarr updated from 5.1.0 to 5.2.0","previousVersion":"5.1.0","newVersion":"5.2.0",
				"eventType":"ApplicationUpdate","instanceName":"Radarr"}`,
			ok: true,
			want: domain.AlertEvent{
				Source: "src", Kind: domain.AlertUpdate, App: "Radarr", Message: "Radarr updated from 5.1.0 to 5.2.0",
				From: "5.1.0", To: "5.2.0",
			},
		},
		{name: "import", body: `{"eventType":"Download","series":{"id":1}}`},
		{name: "test", body: `{"eventType":"Test","instanceName":"Prowlarr"}`},
		{name: "grab", body: `{"eventType":"Grab"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := arr.ParseAlert("src", []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseAlert = %+v, %v\nwant %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
	for name, body := range map[string]string{
		"bad json":            `{`,
		"health without type": `{"eventType":"Health","level":"warning","message":"m"}`,
		"unknown level":       `{"eventType":"Health","type":"X","level":"fatal","message":"m"}`,
		"level ok":            `{"eventType":"Health","type":"X","level":"ok","message":"m"}`,
		"update without new":  `{"eventType":"ApplicationUpdate","message":"m"}`,
	} {
		if _, _, err := arr.ParseAlert("src", []byte(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestHealth(t *testing.T) {
	for _, c := range []struct {
		name, path string
		client     func(base string) *arr.Client
	}{
		{"sonarr/radarr v3", "/api/v3/health", func(b string) *arr.Client { return arr.NewClient(b, arr.StaticKey("k"), "test", 0) }},
		{"prowlarr v1", "/api/v1/health", func(b string) *arr.Client { return arr.NewProwlarrClient(b, arr.StaticKey("k"), "test", 0) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeArr{bodies: map[string]string{c.path: `[
				{"source":"IndexerStatusCheck","type":"warning","message":"Indexers unavailable due to failures: Example","wikiUrl":"https://wiki.example.org/x"},
				{"source":"DownloadClientCheck","type":"error","message":"Unable to communicate","wikiUrl":{"fullUri":"https://wiki.example.org/y"}},
				{"source":"PassingCheck","type":"ok","message":""}
			]`}}
			srv := httptest.NewServer(f)
			defer srv.Close()
			got, err := c.client(srv.URL).Health(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []domain.HealthCheck{
				{Check: "IndexerStatusCheck", Level: domain.LevelWarning, Message: "Indexers unavailable due to failures: Example"},
				{Check: "DownloadClientCheck", Level: domain.LevelError, Message: "Unable to communicate"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Health = %+v", got)
			}
			if k := f.requests()[0].Header.Get("X-Api-Key"); k != "k" {
				t.Errorf("api key %q", k)
			}
		})
	}
	t.Run("error", func(t *testing.T) {
		srv := httptest.NewServer(&fakeArr{status: map[string]int{"/api/v1/health": http.StatusUnauthorized}})
		defer srv.Close()
		_, err := arr.NewProwlarrClient(srv.URL, arr.StaticKey("k"), "test", 0).Health(context.Background())
		var se *arr.StatusError
		if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
			t.Errorf("err = %v", err)
		}
	})
}
