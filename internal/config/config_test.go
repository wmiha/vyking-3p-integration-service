package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validYAML = `
app:
  name: svc
  env: ${APP_ENV:-local}
log:
  level: ${LOG_LEVEL:-info}
  format: json
server:
  shutdown_timeout: 30s
  drain_delay: 5s
  readiness_timeout: 2s
  public:
    enabled: ${PUBLIC_ENABLED:-true}
    addr: "${PUBLIC_ADDR:-:8080}"
    read_header_timeout: 1s
    read_timeout: 2s
    write_timeout: 3s
    idle_timeout: 4s
  internal:
    addr: ":9090"
    read_header_timeout: 1s
    read_timeout: 2s
    write_timeout: 3s
    idle_timeout: 4s
partner_api: {default_limit: 20, max_limit: 100}
` + databaseYAML

const databaseYAML = `
database:
  startup_timeout: 30s
  query_timeout: 5s
  tx_max_retries: 3
  primary:
    host: "${DB_HOST:-127.0.0.1}"
    port: ${DB_PORT:-3306}
    name: vyking
    user: app
    password: "${DB_PASSWORD:-dummy}"
    timezone: UTC
    connect_timeout: 1s
    read_timeout: 2s
    write_timeout: 3s
    pool: {max_open_conns: 10, max_idle_conns: 5, conn_max_lifetime: 30m, conn_max_idle_time: 5m}
  replica:
    enabled: ${DB_REPLICA_ENABLED:-false}
auth:
  issuer: vyking-integration
  audience: vyking-partner-api
  leeway: 30s
  keys:
    - {kid: "${AUTH_KEY_ID:-k1}", secret: "${AUTH_JWT_SECRET:-test-secret-0123456789-0123456789}"}
    - {kid: "${AUTH_PREVIOUS_KEY_ID:-}", secret: "${AUTH_PREVIOUS_JWT_SECRET:-}"}
`

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		check   func(t *testing.T, c *Config)
		wantErr []string
	}{
		{
			name: "defaults applied",
			yaml: validYAML,
			check: func(t *testing.T, c *Config) {
				assert.Equal(t, "svc", c.App.Name)
				assert.Equal(t, EnvLocal, c.App.Env)
				assert.Equal(t, slog.LevelInfo, c.Log.Level)
				assert.True(t, c.Server.Public.Enabled)
				assert.Equal(t, ":8080", c.Server.Public.Addr)
				assert.Equal(t, 30*time.Second, c.Server.ShutdownTimeout)
				assert.Equal(t, 4*time.Second, c.Server.Internal.IdleTimeout)
			},
		},
		{
			name: "env overrides with type coercion",
			yaml: validYAML,
			env:  map[string]string{"APP_ENV": "prod", "LOG_LEVEL": "debug", "PUBLIC_ENABLED": "false"},
			check: func(t *testing.T, c *Config) {
				assert.Equal(t, EnvProd, c.App.Env)
				assert.Equal(t, slog.LevelDebug, c.Log.Level)
				assert.False(t, c.Server.Public.Enabled)
			},
		},
		{
			name: "secret with yaml syntax cannot change structure",
			yaml: validYAML,
			env:  map[string]string{"PUBLIC_ADDR": "x: y # z\nlog: {}"},
			check: func(t *testing.T, c *Config) {
				assert.Equal(t, "x: y # z\nlog: {}", c.Server.Public.Addr)
				assert.Equal(t, "json", c.Log.Format)
			},
		},
		{
			name:    "missing required variables are all reported",
			yaml:    "app:\n  name: ${NAME}\n  env: ${ENV}\n",
			wantErr: []string{"missing required environment variables: ENV, NAME"},
		},
		{
			name:    "unknown key rejected",
			yaml:    validYAML + "unknown_section: true\n",
			wantErr: []string{"field unknown_section not found"},
		},
		{
			name:    "malformed yaml",
			yaml:    "app: [unclosed",
			wantErr: []string{"parse yaml"},
		},
		{
			name:    "wrong type",
			yaml:    "server:\n  shutdown_timeout: soon\n",
			wantErr: []string{"decode"},
		},
		{
			name: "validation errors are joined",
			yaml: `
app: {name: "", env: qa}
log: {level: info, format: xml}
server:
  shutdown_timeout: 5s
  drain_delay: 10s
  public: {enabled: true, addr: ":9090", read_header_timeout: 1s, read_timeout: 1s, write_timeout: 1s, idle_timeout: 1s}
  internal: {addr: ":9090", read_header_timeout: 1s, read_timeout: 0s, write_timeout: 1s, idle_timeout: 1s}
`,
			wantErr: []string{
				"app.name is required",
				`app.env "qa" must be one of`,
				`log.format "xml" must be text or json`,
				"server.drain_delay must be >= 0 and shorter than server.shutdown_timeout",
				"server.readiness_timeout must be positive",
				"server.internal.read_timeout must be positive",
				"server.public.addr and server.internal.addr must differ",
			},
		},
		{
			name: "disabled public server is not validated",
			yaml: `
app: {name: svc, env: local}
log: {level: info, format: text}
server:
  shutdown_timeout: 5s
  drain_delay: 0s
  readiness_timeout: 1s
  public: {enabled: false}
  internal: {addr: ":9090", read_header_timeout: 1s, read_timeout: 1s, write_timeout: 1s, idle_timeout: 1s}
` + databaseYAML,
			check: func(t *testing.T, c *Config) { assert.False(t, c.Server.Public.Enabled) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml), mapLookup(tt.env))
			if len(tt.wantErr) > 0 {
				require.Error(t, err)
				for _, want := range tt.wantErr {
					assert.ErrorContains(t, err, want)
				}
				return
			}
			require.NoError(t, err)
			tt.check(t, cfg)
		})
	}
}

func TestPath(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "defaults to local", want: filepath.Join("config", "local.yaml")},
		{name: "env selects file", env: map[string]string{VarEnv: "staging"}, want: filepath.Join("config", "staging.yaml")},
		{name: "custom dir", env: map[string]string{VarEnv: "prod", VarDir: "/etc/svc"}, want: filepath.Join("/etc/svc", "prod.yaml")},
		{name: "explicit path wins", env: map[string]string{VarEnv: "prod", VarPath: "/run/cfg.yaml"}, want: "/run/cfg.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Path(mapLookup(tt.env)))
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte(validYAML), 0o600))

	cfg, err := Load(mapLookup(map[string]string{VarEnv: "dev", VarDir: dir}))
	require.NoError(t, err)
	assert.Equal(t, "svc", cfg.App.Name)

	_, err = Load(mapLookup(map[string]string{VarEnv: "staging", VarDir: dir}))
	require.ErrorContains(t, err, "read config")

	_, err = Load(mapLookup(map[string]string{VarEnv: "../dev", VarDir: dir}))
	require.ErrorContains(t, err, `APP_ENV="../dev" must be one of local, dev, staging, prod`)

	fixed := filepath.Join(dir, "fixed.yaml")
	require.NoError(t, os.WriteFile(fixed, []byte(strings.Replace(validYAML, "${APP_ENV:-local}", "dev", 1)), 0o600))
	cfg, err = Load(mapLookup(map[string]string{VarEnv: "ignored", VarPath: fixed}))
	require.NoError(t, err, "APP_ENV is irrelevant when CONFIG_PATH is set")
	assert.Equal(t, "svc", cfg.App.Name)
}

// TestShippedConfigs guards the files in /config against typos and drift.
func TestShippedConfigs(t *testing.T) {
	for _, env := range []string{EnvLocal, EnvDev, EnvStaging, EnvProd} {
		t.Run(env, func(t *testing.T) {
			cfg, err := Load(mapLookup(map[string]string{
				VarEnv: env,
				VarDir: filepath.Join("..", "..", "config"),
				// Secrets every non-local environment requires.
				"DB_HOST": "mysql", "DB_NAME": "vyking", "DB_USER": "app", "DB_PASSWORD": "secret",
				"AUTH_KEY_ID": "2026-11", "AUTH_JWT_SECRET": "prod-like-secret-0123456789-0123456789",
			}))
			require.NoError(t, err)
			assert.Equal(t, env, cfg.App.Env)
		})
	}
}

func TestPprofValidation(t *testing.T) {
	tests := []struct {
		name    string
		pprof   string
		wantErr []string
	}{
		{name: "disabled needs no credentials", pprof: "pprof: {enabled: false}"},
		{name: "enabled with credentials", pprof: `pprof: {enabled: true, username: ops, password: "long-enough-secret"}`},
		{
			name:    "enabled without credentials",
			pprof:   "pprof: {enabled: true}",
			wantErr: []string{"pprof.username is required", "pprof.password must be at least 12 characters"},
		},
		{
			name:    "short password",
			pprof:   `pprof: {enabled: true, username: ops, password: "short"}`,
			wantErr: []string{"pprof.password must be at least 12 characters"},
		},
		{
			name:    "negative rates",
			pprof:   "pprof: {enabled: false, block_profile_rate: -1}",
			wantErr: []string{"must be >= 0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(validYAML+tt.pprof+"\n"), mapLookup(nil))
			if len(tt.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tt.wantErr {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

func TestDatabaseConfig(t *testing.T) {
	t.Run("defaults and coercion", func(t *testing.T) {
		cfg, err := Parse([]byte(validYAML), mapLookup(map[string]string{"DB_PORT": "3307"}))
		require.NoError(t, err)
		db := cfg.Database
		assert.Equal(t, 3307, db.Primary.Port)
		assert.Equal(t, "127.0.0.1", db.Primary.Host)
		assert.Equal(t, 10, db.Primary.Pool.MaxOpenConns)
		assert.Equal(t, 30*time.Minute, db.Primary.Pool.ConnMaxLifetime)
		assert.False(t, db.Replica.Enabled, "an unconfigured replica is not validated")
	})

	t.Run("password with yaml syntax", func(t *testing.T) {
		cfg, err := Parse([]byte(validYAML), mapLookup(map[string]string{"DB_PASSWORD": `p@ss: "w#rd"`}))
		require.NoError(t, err)
		assert.Equal(t, `p@ss: "w#rd"`, cfg.Database.Primary.Password)
	})

	t.Run("enabled replica is validated", func(t *testing.T) {
		_, err := Parse([]byte(validYAML), mapLookup(map[string]string{"DB_REPLICA_ENABLED": "true"}))
		require.Error(t, err)
		for _, want := range []string{
			"database.replica.host is required",
			"database.replica.port 0 is out of range",
			"database.replica.pool.max_open_conns must be at least 1",
		} {
			assert.ErrorContains(t, err, want)
		}
	})

	t.Run("invalid primary", func(t *testing.T) {
		bad := strings.NewReplacer(
			"max_open_conns: 10, max_idle_conns: 5", "max_open_conns: 2, max_idle_conns: 5",
			"query_timeout: 5s", "query_timeout: 0s",
		).Replace(validYAML)
		_, err := Parse([]byte(bad), mapLookup(map[string]string{"DB_PORT": "70000"}))
		require.Error(t, err)
		for _, want := range []string{
			"database.query_timeout must be positive",
			"database.primary.port 70000 is out of range",
			"database.primary.pool.max_idle_conns must be between 0 and max_open_conns",
		} {
			assert.ErrorContains(t, err, want)
		}
	})
}

func TestAuthConfig(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		yaml     string // replaces the default auth section when set
		wantKeys []string
		wantErr  []string
	}{
		{name: "empty rotation slot is ignored", wantKeys: []string{"k1"}},
		{
			name:     "rotation with previous key",
			env:      map[string]string{"AUTH_PREVIOUS_KEY_ID": "k0", "AUTH_PREVIOUS_JWT_SECRET": "old-secret-0123456789-0123456789-01"},
			wantKeys: []string{"k1", "k0"},
		},
		{
			name:    "short secret",
			env:     map[string]string{"AUTH_JWT_SECRET": "too-short"},
			wantErr: []string{"auth.keys[0].secret must be at least 32 bytes"},
		},
		{
			name:    "secret without kid",
			env:     map[string]string{"AUTH_PREVIOUS_JWT_SECRET": "old-secret-0123456789-0123456789-01"},
			wantErr: []string{"auth.keys[1].kid is required"},
		},
		{
			name:    "duplicated kid",
			env:     map[string]string{"AUTH_PREVIOUS_KEY_ID": "k1", "AUTH_PREVIOUS_JWT_SECRET": "old-secret-0123456789-0123456789-01"},
			wantErr: []string{`auth.keys[1].kid "k1" is duplicated`},
		},
		{
			name:    "no keys and unbounded leeway",
			yaml:    "auth: {issuer: i, audience: a, leeway: 1h}\n",
			wantErr: []string{"auth.keys must contain at least one key", "auth.leeway must be between 0 and 5m0s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := validYAML
			if tt.yaml != "" {
				raw = raw[:strings.Index(raw, "auth:")] + tt.yaml
			}
			cfg, err := Parse([]byte(raw), mapLookup(tt.env))
			if len(tt.wantErr) > 0 {
				for _, want := range tt.wantErr {
					assert.ErrorContains(t, err, want)
				}
				return
			}
			require.NoError(t, err)
			var ids []string
			for _, k := range cfg.Auth.ActiveKeys() {
				ids = append(ids, k.ID)
			}
			assert.Equal(t, tt.wantKeys, ids)
		})
	}
}

func TestPartnerAPIConfig(t *testing.T) {
	tests := []struct {
		name    string
		limits  string
		wantErr bool
	}{
		{name: "valid", limits: "{default_limit: 20, max_limit: 100}"},
		{name: "default above max", limits: "{default_limit: 50, max_limit: 10}", wantErr: true},
		{name: "zero default", limits: "{default_limit: 0, max_limit: 10}", wantErr: true},
		{name: "max above ceiling", limits: "{default_limit: 20, max_limit: 1000}", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(validYAML, "partner_api: {default_limit: 20, max_limit: 100}", "partner_api: "+tt.limits, 1)
			cfg, err := Parse([]byte(raw), mapLookup(nil))
			if tt.wantErr {
				assert.ErrorContains(t, err, "partner_api limits must satisfy")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, PartnerAPI{DefaultLimit: 20, MaxLimit: 100}, cfg.PartnerAPI)
		})
	}

	// A sync-only instance needs no partner API settings.
	raw := strings.Replace(validYAML, "partner_api: {default_limit: 20, max_limit: 100}", "", 1)
	_, err := Parse([]byte(raw), mapLookup(map[string]string{"PUBLIC_ENABLED": "false"}))
	assert.NoError(t, err)
}

func TestCacheConfig(t *testing.T) {
	cfg, err := Parse([]byte(validYAML+"cache: {enabled: true, ttl: 15s, max_entries: 100, sweep_interval: 1m}\n"), mapLookup(nil))
	require.NoError(t, err)
	assert.Equal(t, Cache{Enabled: true, TTL: 15 * time.Second, MaxEntries: 100, SweepInterval: time.Minute}, cfg.Cache)

	_, err = Parse([]byte(validYAML+"cache: {enabled: true, ttl: 0s, max_entries: 100, sweep_interval: 1m}\n"), mapLookup(nil))
	assert.ErrorContains(t, err, "cache.ttl, cache.max_entries and cache.sweep_interval must be positive")

	_, err = Parse([]byte(validYAML+"cache: {enabled: false}\n"), mapLookup(nil))
	assert.NoError(t, err, "a disabled cache needs no settings")
}
