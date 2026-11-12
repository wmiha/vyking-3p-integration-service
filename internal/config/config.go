// Package config loads the service configuration from a per-environment YAML
// file and expands ${VAR} / ${VAR:-default} placeholders from the process
// environment, so the same files work locally, in compose and as a mounted
// Kubernetes ConfigMap with secrets injected as environment variables.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Supported deployment environments; each has a config/<env>.yaml file.
const (
	EnvLocal   = "local"
	EnvDev     = "dev"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

var environments = []string{EnvLocal, EnvDev, EnvStaging, EnvProd}

// Environment variables that select the configuration file.
const (
	VarEnv  = "APP_ENV"     // local|dev|staging|prod, defaults to local
	VarDir  = "CONFIG_DIR"  // directory holding <env>.yaml, defaults to "config"
	VarPath = "CONFIG_PATH" // explicit file path, overrides VarEnv and VarDir
)

// LookupFunc has the signature of os.LookupEnv and is injected for testing.
type LookupFunc func(key string) (string, bool)

type Config struct {
	App    App    `yaml:"app"`
	Log    Log    `yaml:"log"`
	Server Server `yaml:"server"`
	Pprof  Pprof  `yaml:"pprof"`
}

// Pprof controls the runtime profiler on the internal server.
type Pprof struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// BlockProfileRate is passed to runtime.SetBlockProfileRate (0 disables).
	BlockProfileRate int `yaml:"block_profile_rate"`
	// MutexProfileFraction is passed to runtime.SetMutexProfileFraction (0 disables).
	MutexProfileFraction int `yaml:"mutex_profile_fraction"`
}

// minPprofPasswordLen rejects trivially guessable profiler passwords.
const minPprofPasswordLen = 12

type App struct {
	Name string `yaml:"name"`
	Env  string `yaml:"env"`
}

type Log struct {
	Level  slog.Level `yaml:"level"`
	Format string     `yaml:"format"` // text|json
}

type Server struct {
	// ShutdownTimeout bounds the whole graceful shutdown sequence.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// DrainDelay keeps serving after readiness turns unhealthy so load
	// balancers can stop routing traffic before listeners close.
	DrainDelay time.Duration `yaml:"drain_delay"`
	// ReadinessTimeout bounds the dependency checks behind /ready.
	ReadinessTimeout time.Duration `yaml:"readiness_timeout"`
	Public           HTTPServer    `yaml:"public"`
	Internal         HTTPServer    `yaml:"internal"`
}

type HTTPServer struct {
	// Enabled is only honoured for the public server, which allows running
	// the binary as a sync-only worker.
	Enabled           bool          `yaml:"enabled"`
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
}

// Path returns the configuration file to load based on the environment.
func Path(lookup LookupFunc) string {
	if p, ok := lookup(VarPath); ok && p != "" {
		return p
	}
	env, ok := lookup(VarEnv)
	if !ok || env == "" {
		env = EnvLocal
	}
	dir, ok := lookup(VarDir)
	if !ok || dir == "" {
		dir = "config"
	}
	return filepath.Join(dir, env+".yaml")
}

// Load reads, expands, decodes and validates the configuration file selected
// by Path.
func Load(lookup LookupFunc) (*Config, error) {
	if p, _ := lookup(VarPath); p == "" {
		if env, _ := lookup(VarEnv); env != "" && !slices.Contains(environments, env) {
			return nil, fmt.Errorf("%s=%q must be one of %s", VarEnv, env, strings.Join(environments, ", "))
		}
	}
	path := Path(lookup)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw, lookup)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse expands placeholders in raw YAML and decodes it strictly: unknown keys
// are rejected so typos in tuning parameters fail fast instead of silently
// falling back to zero values.
//
// Placeholders are expanded per scalar node rather than on the raw text, so a
// secret containing YAML syntax (":", "#", quotes) can never alter the
// document structure.
func Parse(raw []byte, lookup LookupFunc) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	var missing []string
	if err := expandNode(&root, lookup, &missing); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("missing required environment variables: %s",
			strings.Join(slices.Compact(missing), ", "))
	}

	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("re-encode yaml: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	return &cfg, nil
}

func expandNode(n *yaml.Node, lookup LookupFunc, missing *[]string) error {
	if n.Kind == yaml.ScalarNode {
		val, miss, err := expand(n.Value, lookup)
		if err != nil {
			return err
		}
		*missing = append(*missing, miss...)
		if val != n.Value {
			n.Value = val
			// A plain (unquoted) placeholder takes the type of its expanded
			// value, so `port: ${DB_PORT:-3306}` decodes into an int. Quoted
			// placeholders stay strings.
			if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
				n.Tag = ""
			}
		}
		return nil
	}
	for _, c := range n.Content {
		if err := expandNode(c, lookup, missing); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports every invalid setting at once.
func (c *Config) Validate() error {
	var errs []error
	if c.App.Name == "" {
		errs = append(errs, errors.New("app.name is required"))
	}
	if !slices.Contains(environments, c.App.Env) {
		errs = append(errs, fmt.Errorf("app.env %q must be one of local, dev, staging, prod", c.App.Env))
	}
	if c.Log.Format != "text" && c.Log.Format != "json" {
		errs = append(errs, fmt.Errorf("log.format %q must be text or json", c.Log.Format))
	}
	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.Pprof.validate()...)
	return errors.Join(errs...)
}

func (p *Pprof) validate() []error {
	var errs []error
	if p.Enabled {
		if p.Username == "" {
			errs = append(errs, errors.New("pprof.username is required when pprof is enabled"))
		}
		if len(p.Password) < minPprofPasswordLen {
			errs = append(errs, fmt.Errorf("pprof.password must be at least %d characters when pprof is enabled", minPprofPasswordLen))
		}
	}
	if p.BlockProfileRate < 0 || p.MutexProfileFraction < 0 {
		errs = append(errs, errors.New("pprof.block_profile_rate and pprof.mutex_profile_fraction must be >= 0"))
	}
	return errs
}

func (s *Server) validate() []error {
	var errs []error
	if s.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be positive"))
	}
	if s.DrainDelay < 0 || s.DrainDelay >= s.ShutdownTimeout {
		errs = append(errs, errors.New("server.drain_delay must be >= 0 and shorter than server.shutdown_timeout"))
	}
	if s.ReadinessTimeout <= 0 {
		errs = append(errs, errors.New("server.readiness_timeout must be positive"))
	}
	if s.Public.Enabled {
		errs = append(errs, s.Public.validate("server.public")...)
	}
	errs = append(errs, s.Internal.validate("server.internal")...)
	if s.Public.Enabled && s.Public.Addr == s.Internal.Addr {
		errs = append(errs, errors.New("server.public.addr and server.internal.addr must differ"))
	}
	return errs
}

func (h *HTTPServer) validate(prefix string) []error {
	var errs []error
	if h.Addr == "" {
		errs = append(errs, fmt.Errorf("%s.addr is required", prefix))
	}
	for name, d := range map[string]time.Duration{
		"read_header_timeout": h.ReadHeaderTimeout,
		"read_timeout":        h.ReadTimeout,
		"write_timeout":       h.WriteTimeout,
		"idle_timeout":        h.IdleTimeout,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s.%s must be positive", prefix, name))
		}
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}
