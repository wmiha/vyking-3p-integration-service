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
	App        App        `yaml:"app"`
	Log        Log        `yaml:"log"`
	Server     Server     `yaml:"server"`
	Pprof      Pprof      `yaml:"pprof"`
	Database   Database   `yaml:"database"`
	Auth       Auth       `yaml:"auth"`
	PartnerAPI PartnerAPI `yaml:"partner_api"`
	Cache      Cache      `yaml:"cache"`
	Acme       Acme       `yaml:"acme"`
	Sync       Sync       `yaml:"sync"`
}

// Acme configures the Acme Support API client.
type Acme struct {
	BaseURL string `yaml:"base_url"`
	// RequestTimeout bounds one HTTP attempt; Acme answers may take 30s.
	RequestTimeout    time.Duration `yaml:"request_timeout"`
	MaxAttempts       int           `yaml:"max_attempts"`
	BaseBackoff       time.Duration `yaml:"base_backoff"`
	MaxBackoff        time.Duration `yaml:"max_backoff"`
	DefaultRetryAfter time.Duration `yaml:"default_retry_after"`
	// RatePerMinute and Burst shape the per-brand token bucket.
	RatePerMinute    int           `yaml:"rate_per_minute"`
	Burst            int           `yaml:"burst"`
	BreakerThreshold int           `yaml:"breaker_threshold"`
	BreakerCooldown  time.Duration `yaml:"breaker_cooldown"`
	AuthCooldown     time.Duration `yaml:"auth_cooldown"`
	Brands           []AcmeBrand   `yaml:"brands"`
}

// AcmeBrand holds one brand's Acme Support API key.
type AcmeBrand struct {
	BrandID uint32 `yaml:"brand_id"`
	APIKey  string `yaml:"api_key"`
}

// ConfiguredBrands returns the brands with an API key; entries without a key
// are empty slots (the brand is not synced).
func (a Acme) ConfiguredBrands() []AcmeBrand {
	var out []AcmeBrand
	for _, b := range a.Brands {
		if b.APIKey != "" {
			out = append(out, b)
		}
	}
	return out
}

// Sync configures the scheduled contact sync.
type Sync struct {
	Enabled            bool          `yaml:"enabled"`
	Interval           time.Duration `yaml:"interval"`
	BatchPerBrand      int           `yaml:"batch_per_brand"`
	WorkersPerBrand    int           `yaml:"workers_per_brand"`
	JobTimeout         time.Duration `yaml:"job_timeout"`
	LockName           string        `yaml:"lock_name"`
	LockSessionTimeout time.Duration `yaml:"lock_session_timeout"`
	LockCheckInterval  time.Duration `yaml:"lock_check_interval"`
	BackoffInitial     time.Duration `yaml:"backoff_initial"`
	BackoffMax         time.Duration `yaml:"backoff_max"`
	LookupBeforeCreate bool          `yaml:"lookup_before_create"`
}

// Cache configures the short-lived partner lookup cache.
type Cache struct {
	Enabled bool `yaml:"enabled"`
	// TTL is how stale a cached transaction page may be.
	TTL time.Duration `yaml:"ttl"`
	// MaxEntries bounds memory use; each entry is one page.
	MaxEntries int `yaml:"max_entries"`
	// SweepInterval is how often expired entries are freed.
	SweepInterval time.Duration `yaml:"sweep_interval"`
}

// PartnerAPI tunes the public transactions endpoint.
type PartnerAPI struct {
	// DefaultLimit is the page size when the partner sends no limit.
	DefaultLimit int `yaml:"default_limit"`
	// MaxLimit caps the page size a partner may request.
	MaxLimit int `yaml:"max_limit"`
}

// maxPageSize is a hard ceiling protecting the database from huge pages.
const maxPageSize = 500

// Auth configures partner bearer token verification (HS256 JWTs, see
// internal/auth). Several keys may be active at once to rotate them without
// downtime; the first active key is the current signing key.
type Auth struct {
	Issuer   string        `yaml:"issuer"`
	Audience string        `yaml:"audience"`
	Leeway   time.Duration `yaml:"leeway"`
	Keys     []AuthKey     `yaml:"keys"`
}

type AuthKey struct {
	ID     string `yaml:"kid"`
	Secret string `yaml:"secret"`
}

// minAuthSecretLen follows RFC 7518: HS256 keys must be at least 256 bits.
const minAuthSecretLen = 32

// maxAuthLeeway bounds tolerated clock skew; larger values would extend the
// lifetime of expired tokens.
const maxAuthLeeway = 5 * time.Minute

// ActiveKeys returns the configured keys, skipping empty rotation slots.
func (a Auth) ActiveKeys() []AuthKey {
	var keys []AuthKey
	for _, k := range a.Keys {
		if k.ID != "" || k.Secret != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// Database configures the MySQL connection pools. Writes and all sync reads
// use the primary; partner API reads use the replica when it is enabled.
type Database struct {
	// StartupTimeout is how long start-up waits for the database to answer.
	StartupTimeout time.Duration `yaml:"startup_timeout"`
	// QueryTimeout bounds a single repository call.
	QueryTimeout time.Duration `yaml:"query_timeout"`
	// TxMaxRetries re-runs transactions aborted by deadlocks or lock waits.
	TxMaxRetries int    `yaml:"tx_max_retries"`
	Primary      DBConn `yaml:"primary"`
	Replica      DBConn `yaml:"replica"`
}

type DBConn struct {
	// Enabled is only honoured for the replica; the primary is mandatory.
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// Timezone of the zone-less DATETIME columns.
	Timezone       string        `yaml:"timezone"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	Pool           DBPool        `yaml:"pool"`
}

type DBPool struct {
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
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
	errs = append(errs, c.Database.validate()...)
	errs = append(errs, c.Auth.validate()...)
	if c.Sync.Enabled {
		errs = append(errs, c.Sync.validate()...)
		errs = append(errs, c.Acme.validate()...)
	}
	if c.Cache.Enabled && (c.Cache.TTL <= 0 || c.Cache.MaxEntries < 1 || c.Cache.SweepInterval <= 0) {
		errs = append(errs, errors.New("cache.ttl, cache.max_entries and cache.sweep_interval must be positive when the cache is enabled"))
	}
	if c.Server.Public.Enabled && (c.PartnerAPI.DefaultLimit < 1 || c.PartnerAPI.DefaultLimit > c.PartnerAPI.MaxLimit || c.PartnerAPI.MaxLimit > maxPageSize) {
		errs = append(errs, fmt.Errorf("partner_api limits must satisfy 1 <= default_limit <= max_limit <= %d", maxPageSize))
	}
	return errors.Join(errs...)
}

func (a *Auth) validate() []error {
	var errs []error
	if a.Issuer == "" || a.Audience == "" {
		errs = append(errs, errors.New("auth.issuer and auth.audience are required"))
	}
	if a.Leeway < 0 || a.Leeway > maxAuthLeeway {
		errs = append(errs, fmt.Errorf("auth.leeway must be between 0 and %s", maxAuthLeeway))
	}
	keys := a.ActiveKeys()
	if len(keys) == 0 {
		errs = append(errs, errors.New("auth.keys must contain at least one key"))
	}
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		if k.ID == "" {
			errs = append(errs, fmt.Errorf("auth.keys[%d].kid is required", i))
		}
		if len(k.Secret) < minAuthSecretLen {
			errs = append(errs, fmt.Errorf("auth.keys[%d].secret must be at least %d bytes", i, minAuthSecretLen))
		}
		if seen[k.ID] {
			errs = append(errs, fmt.Errorf("auth.keys[%d].kid %q is duplicated", i, k.ID))
		}
		seen[k.ID] = true
	}
	return errs
}

func (d *Database) validate() []error {
	var errs []error
	if d.StartupTimeout <= 0 {
		errs = append(errs, errors.New("database.startup_timeout must be positive"))
	}
	if d.QueryTimeout <= 0 {
		errs = append(errs, errors.New("database.query_timeout must be positive"))
	}
	if d.TxMaxRetries < 0 {
		errs = append(errs, errors.New("database.tx_max_retries must be >= 0"))
	}
	errs = append(errs, d.Primary.validate("database.primary")...)
	if d.Replica.Enabled {
		errs = append(errs, d.Replica.validate("database.replica")...)
	}
	return errs
}

func (c *DBConn) validate(prefix string) []error {
	var errs []error
	for name, v := range map[string]string{"host": c.Host, "name": c.Name, "user": c.User, "timezone": c.Timezone} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s.%s is required", prefix, name))
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("%s.port %d is out of range", prefix, c.Port))
	}
	for name, d := range map[string]time.Duration{
		"connect_timeout": c.ConnectTimeout,
		"read_timeout":    c.ReadTimeout,
		"write_timeout":   c.WriteTimeout,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s.%s must be positive", prefix, name))
		}
	}
	p := c.Pool
	if p.MaxOpenConns < 1 {
		errs = append(errs, fmt.Errorf("%s.pool.max_open_conns must be at least 1", prefix))
	}
	if p.MaxIdleConns < 0 || p.MaxIdleConns > p.MaxOpenConns {
		errs = append(errs, fmt.Errorf("%s.pool.max_idle_conns must be between 0 and max_open_conns", prefix))
	}
	if p.ConnMaxLifetime < 0 || p.ConnMaxIdleTime < 0 {
		errs = append(errs, fmt.Errorf("%s.pool lifetimes must be >= 0", prefix))
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
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

func (s *Sync) validate() []error {
	var errs []error
	for name, d := range map[string]time.Duration{
		"interval": s.Interval, "job_timeout": s.JobTimeout, "lock_session_timeout": s.LockSessionTimeout,
		"lock_check_interval": s.LockCheckInterval, "backoff_initial": s.BackoffInitial,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("sync.%s must be positive", name))
		}
	}
	if s.BackoffMax < s.BackoffInitial {
		errs = append(errs, errors.New("sync.backoff_max must be >= sync.backoff_initial"))
	}
	if s.BatchPerBrand < 1 || s.WorkersPerBrand < 1 {
		errs = append(errs, errors.New("sync.batch_per_brand and sync.workers_per_brand must be at least 1"))
	}
	if s.LockName == "" {
		errs = append(errs, errors.New("sync.lock_name is required"))
	}
	if s.LockSessionTimeout <= s.LockCheckInterval {
		errs = append(errs, errors.New("sync.lock_session_timeout must exceed sync.lock_check_interval"))
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

func (a *Acme) validate() []error {
	var errs []error
	if a.BaseURL == "" {
		errs = append(errs, errors.New("acme.base_url is required"))
	}
	for name, d := range map[string]time.Duration{
		"request_timeout": a.RequestTimeout, "base_backoff": a.BaseBackoff, "max_backoff": a.MaxBackoff,
		"default_retry_after": a.DefaultRetryAfter, "breaker_cooldown": a.BreakerCooldown, "auth_cooldown": a.AuthCooldown,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("acme.%s must be positive", name))
		}
	}
	if a.MaxAttempts < 1 || a.RatePerMinute < 1 || a.Burst < 1 || a.BreakerThreshold < 1 {
		errs = append(errs, errors.New("acme.max_attempts, rate_per_minute, burst and breaker_threshold must be at least 1"))
	}
	seen := map[uint32]bool{}
	for _, b := range a.Brands {
		if b.BrandID == 0 || seen[b.BrandID] {
			errs = append(errs, fmt.Errorf("acme.brands: brand_id %d is missing or duplicated", b.BrandID))
		}
		seen[b.BrandID] = true
	}
	if len(a.ConfiguredBrands()) == 0 {
		errs = append(errs, errors.New("sync is enabled but no brand has an Acme Support API key (acme.brands)"))
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}
