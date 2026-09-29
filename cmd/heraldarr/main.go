// Command heraldarr announces new media from Sonarr and Radarr to Discord.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wtwerner/heraldarr/internal/app"
	"github.com/wtwerner/heraldarr/internal/config"
	"github.com/wtwerner/heraldarr/internal/domain"
)

var version = "dev" // set by -ldflags at build time

const usage = `heraldarr announces new media from Sonarr and Radarr to Discord.

Usage:
  heraldarr serve                      run the webhook receiver and poster
  heraldarr validate                   check the configuration and exit
  heraldarr flush                      post everything pending now (asks the running server)
  heraldarr health                     exit 0 if the running server is healthy
  heraldarr preview SOURCE ID[,ID…] [S02|S02E05,S02E06] [-to DESTINATION]
                                       render items already on disk as if they just arrived
                                       (one series, or several movies);
                                       prints the Discord JSON, or posts it to a non-public
                                       destination with -to
  heraldarr import-legacy DIR          import an arr-discord data/ folder (stop the server first)
  heraldarr version

Every command takes -config PATH (default $HERALDARR_CONFIG or /config/config.yaml).
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "heraldarr:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command")
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Print(usage)
			return nil
		}
	}
	fs := flag.NewFlagSet("heraldarr", flag.ContinueOnError)
	configPath := fs.String("config", envOr("HERALDARR_CONFIG", "/config/config.yaml"), "configuration file")
	to := fs.String("to", "", "preview: destination to post to")
	if err := fs.Parse(interspersed(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fmt.Print(usage)
		return errors.New("no command")
	}
	cmd, pos := fs.Arg(0), fs.Args()[1:]

	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log) // packages that log through slog's default use the same handler
	switch cmd {
	case "validate":
		fmt.Printf("%s: ok (%d sources, %d destinations, %d routes)\n", *configPath,
			len(cfg.Sources), len(cfg.Destinations), len(cfg.Routes))
		return nil
	case "flush":
		return callServer(cfg, http.MethodPost, "/flush")
	case "health":
		return callServer(cfg, http.MethodGet, "/health")
	case "serve":
		return serve(cfg, log)
	case "preview":
		return preview(cfg, log, pos, *to)
	case "import-legacy":
		if len(pos) != 1 {
			return errors.New("import-legacy DIR")
		}
		svc, err := app.FromConfig(cfg, version, log)
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		n, err := svc.Store.ImportLegacy(context.Background(), pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("imported %d batches, %d posted keys, %d cache entries, %d history lines\n",
			n.Batches, n.Posted, n.Cache, n.History)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(cfg *config.Config, log *slog.Logger) error {
	svc, err := app.FromConfig(cfg, version, log)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr: cfg.Server.Listen, Handler: svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	ran := make(chan struct{})
	go func() { svc.Run(ctx); close(ran) }()
	names := make([]string, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		names = append(names, s.Name)
	}
	t := cfg.Timing
	log.Info("heraldarr listening", "version", version, "addr", cfg.Server.Listen, "sources", strings.Join(names, ","),
		"quiet_episodes", t.QuietEpisodes, "quiet_backlog", t.QuietBacklog, "quiet_movies", t.QuietMovies,
		"digest_from", t.DigestFrom, "max_hold", t.MaxHold, "auth", cfg.Server.Auth != nil, "media_server", cfg.MediaServer != nil)
	select {
	case err = <-errc:
		stop() // the listener failed: stop the flush loop too
	case <-ctx.Done():
		log.Info("shutting down")
		// Longer than an *arr lookup (20 s) a webhook may be waiting on.
		shut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = srv.Shutdown(shut)
	}
	<-ran // the flush loop records what it sent before the store closes
	return err
}

func preview(cfg *config.Config, log *slog.Logger, pos []string, to string) error {
	if len(pos) < 2 || len(pos) > 3 {
		return errors.New("preview SOURCE ID[,ID…] [S02|S02E05,S02E06] [-to DESTINATION]")
	}
	svc, err := app.FromConfig(cfg, version, log)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	source := pos[0]
	client, ok := svc.Arr[source]
	if !ok {
		return fmt.Errorf("unknown source %q", source)
	}
	ids := strings.Split(pos[1], ",")
	isTV := svc.Sources[source].Kind == domain.KindTV
	switch {
	case isTV && len(ids) > 1:
		return errors.New("preview: one series at a time")
	case !isTV && len(pos) == 3:
		return fmt.Errorf("preview: %q is a movie source; season/episode selectors are for series", source)
	}
	var dest *domain.Destination
	if to != "" {
		d, ok := svc.Destinations[to]
		if !ok {
			return fmt.Errorf("unknown destination %q", to)
		}
		dest = &d
	}
	ctx := context.Background()
	var imps []domain.Import
	for _, s := range ids {
		id, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("bad id %q: %w", s, err)
		}
		var imp domain.Import
		if svc.Sources[source].Kind == domain.KindTV {
			sel := ""
			if len(pos) == 3 {
				sel = pos[2]
			}
			imp, err = client.SeriesImport(ctx, source, id, sel)
		} else {
			imp, err = client.MovieImport(ctx, source, id)
		}
		if err != nil {
			return err
		}
		imps = append(imps, imp)
	}
	layouts, err := svc.Preview(ctx, source, imps, dest)
	if err != nil {
		return err
	}
	if dest != nil {
		fmt.Printf("posted to %s\n", dest.Name)
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	for _, l := range layouts {
		if err := enc.Encode(l[0].Body); err != nil {
			return err
		}
	}
	return nil
}

// callServer makes a request to the running server on this machine.
func callServer(cfg *config.Config, method, path string) error {
	host, port, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen: %w", err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1" // listening on every address: loopback is one of them
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+net.JoinHostPort(host, port)+path, http.NoBody)
	if err != nil {
		return err
	}
	if a := cfg.Server.Auth; a != nil {
		req.SetBasicAuth(a.Username, a.Password.Value())
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if path == "/flush" {
		fmt.Println("flush requested; see the logs")
	}
	return nil
}

// interspersed moves flags to the front wherever they appear, so both
// "heraldarr -config c.yaml serve" and "heraldarr preview radarr 5 -to private" parse.
func interspersed(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func logLevel() slog.Level {
	if os.Getenv("HERALDARR_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
