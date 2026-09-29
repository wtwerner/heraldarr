package config

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Secret is a credential written inline, or (preferred) read from a file or environment variable:
//
//	api_key: abc123
//	api_key: {file: /config/secrets/sonarr_api_key}
//	api_key: {env: SONARR_API_KEY}
//
// It is resolved once at load. String() never reveals the value.
type Secret struct {
	value  string
	source string // "inline", "file:<path>", "env:<name>"
	err    error
}

func (s *Secret) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		s.value, s.source = strings.TrimSpace(n.Value), "inline"
	case yaml.MappingNode:
		var ref struct {
			File string `yaml:"file"`
			Env  string `yaml:"env"`
		}
		if err := n.Decode(&ref); err != nil {
			return err
		}
		switch {
		case (ref.File == "") == (ref.Env == ""):
			return fmt.Errorf("line %d: secret needs exactly one of file or env", n.Line)
		case ref.File != "":
			s.source = "file:" + ref.File
			b, err := os.ReadFile(ref.File)
			if err != nil {
				s.err = err // reported by Validate with every other problem
				return nil  //nolint:nilerr // see above
			}
			s.value = strings.TrimSpace(string(b))
		default:
			s.source = "env:" + ref.Env
			s.value = strings.TrimSpace(os.Getenv(ref.Env))
		}
		if s.value == "" && s.err == nil {
			s.err = fmt.Errorf("%s is empty", s.source)
		}
	default:
		return fmt.Errorf("line %d: secret must be a string or {file: …} / {env: …}", n.Line)
	}
	return nil
}

// Literal makes a resolved secret (tests, programmatic config).
func Literal(v string) Secret { return Secret{value: v, source: "inline"} }

func (s Secret) Value() string  { return s.value }
func (s Secret) IsZero() bool   { return s.source == "" }
func (s Secret) Err() error     { return s.err }
func (s Secret) String() string { return "secret(" + s.source + ")" }
