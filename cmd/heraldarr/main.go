// Command heraldarr announces new media from Sonarr and Radarr to Discord.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/wtwerner/heraldarr/internal/config"
)

var version = "dev" // set by -ldflags at build time

const usage = `heraldarr announces new media from Sonarr and Radarr to Discord.

Usage:
  heraldarr serve    [-config path]   run the webhook receiver and poster
  heraldarr validate [-config path]   check the configuration and exit
  heraldarr version
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
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := fs.String("config", envOr("HERALDARR_CONFIG", "/config/config.yaml"), "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "validate":
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		fmt.Printf("%s: ok (%d sources, %d destinations, %d routes)\n", *configPath,
			len(cfg.Sources), len(cfg.Destinations), len(cfg.Routes))
		return nil
	case "serve":
		return errors.New("serve: not implemented yet (Wave 2)")
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
