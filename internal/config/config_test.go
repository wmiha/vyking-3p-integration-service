package config

import (
	"log/slog"
	"maps"
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
		{name: "default file", want: DefaultPath},
		{name: "empty path falls back", env: map[string]string{VarPath: ""}, want: DefaultPath},
		{name: "explicit path wins", env: map[string]string{VarPath: "/run/cfg.yaml"}, want: "/run/cfg.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Path(mapLookup(tt.env)))
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o600))

	cfg, err := Load(mapLookup(map[string]string{VarPath: path, "APP_ENV": "dev"}))
	require.NoError(t, err)
	assert.Equal(t, "svc", cfg.App.Name)
	assert.Equal(t, EnvDev, cfg.App.Env)

	_, err = Load(mapLookup(map[string]string{VarPath: filepath.Join(dir, "missing.yaml")}))
	require.ErrorContains(t, err, "read config")

	_, err = Load(mapLookup(map[string]string{VarPath: path, "APP_ENV": "qa"}))
	require.ErrorContains(t, err, `app.env "qa" must be one of local, dev, staging, prod`)
}

// TestShippedConfig guards config/config.yaml and config/local.env against
// typos and drift, and checks that secrets and endpoints have no defaults.
func TestShippedConfig(t *testing.T) {
	shipped := filepath.Join("..", "..", "config", "config.yaml")
	prodLike := map[string]string{
		VarPath: shipped, "APP_ENV": EnvProd,
		"DB_HOST": "mysql", "DB_NAME": "vyking", "DB_USER": "app", "DB_PASSWORD": "secret",
		"AUTH_KEY_ID": "2026-11", "AUTH_JWT_SECRET": "prod-like-secret-0123456789-0123456789",
		"ACME_BASE_URL": "https://api.acme-support.example", "ACME_API_KEY_BRAND_1": "k1",
	}

	t.Run("local", func(t *testing.T) {
		env := readEnvFile(t, filepath.Join("..", "..", "config", "local.env"))
		env[VarPath] = shipped
		cfg, err := Load(mapLookup(env))
		require.NoError(t, err)
		assert.Equal(t, EnvLocal, cfg.App.Env)
		assert.Equal(t, "text", cfg.Log.Format)
		assert.Zero(t, cfg.Server.DrainDelay)
		assert.True(t, cfg.Pprof.Enabled)
		assert.Equal(t, 10, cfg.Database.Primary.Pool.MaxOpenConns)
		assert.Len(t, cfg.Acme.ConfiguredBrands(), 2, "brand 3 stays unconfigured locally")
	})

	t.Run("prod defaults", func(t *testing.T) {
		cfg, err := Load(mapLookup(prodLike))
		require.NoError(t, err)
		assert.Equal(t, EnvProd, cfg.App.Env)
		assert.Equal(t, slog.LevelInfo, cfg.Log.Level)
		assert.Equal(t, "json", cfg.Log.Format)
		assert.Equal(t, 5*time.Second, cfg.Server.DrainDelay)
		assert.False(t, cfg.Pprof.Enabled)
		assert.False(t, cfg.Database.Replica.Enabled)
		assert.Equal(t, 40, cfg.Database.Primary.Pool.MaxOpenConns)
		assert.Equal(t, 20, cfg.Database.Replica.Pool.MaxIdleConns)
	})

	t.Run("per-environment overrides", func(t *testing.T) {
		env := maps.Clone(prodLike)
		env["APP_ENV"], env["DB_POOL_MAX_OPEN"], env["DB_POOL_MAX_IDLE"] = EnvStaging, "20", "10"
		cfg, err := Load(mapLookup(env))
		require.NoError(t, err)
		assert.Equal(t, EnvStaging, cfg.App.Env)
		assert.Equal(t, 20, cfg.Database.Primary.Pool.MaxOpenConns)
		assert.Equal(t, 10, cfg.Database.Primary.Pool.MaxIdleConns)
	})

	t.Run("secrets and endpoints have no defaults", func(t *testing.T) {
		_, err := Load(mapLookup(map[string]string{VarPath: shipped}))
		require.EqualError(t, err, "config "+shipped+": missing required environment variables: "+
			"ACME_BASE_URL, APP_ENV, AUTH_JWT_SECRET, AUTH_KEY_ID, DB_HOST, DB_NAME, DB_PASSWORD, DB_USER")
	})
}

// readEnvFile parses KEY=value lines, skipping blank lines and comments.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	env := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		require.True(t, ok, "not a KEY=value line: %q", line)
		env[k] = v
	}
	return env
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

const syncYAML = `
acme:
  base_url: http://acme
  request_timeout: 35s
  max_attempts: 3
  base_backoff: 1s
  max_backoff: 30s
  default_retry_after: 60s
  rate_per_minute: 55
  burst: 10
  breaker_threshold: 5
  breaker_cooldown: 1m
  auth_cooldown: 15m
  brands:
    - {brand_id: 1, api_key: "${KEY_1:-}"}
    - {brand_id: 2, api_key: "${KEY_2:-}"}
sync:
  enabled: ${SYNC_ENABLED:-true}
  interval: 30s
  batch_per_brand: 50
  workers_per_brand: 4
  job_timeout: 2m
  lock_name: sync
  lock_session_timeout: 300s
  lock_check_interval: 5s
  backoff_initial: 1m
  backoff_max: 1h
`

func TestSyncConfig(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		replace    [2]string
		wantBrands []uint32
		wantErr    string
	}{
		{name: "brands without a key are skipped", wantBrands: []uint32{1}},
		{name: "both brands", env: map[string]string{"KEY_2": "k2"}, wantBrands: []uint32{1, 2}},
		{name: "no key at all", env: map[string]string{"KEY_1": ""},
			wantErr: "sync is enabled but no brand has an Acme Support API key"},
		{name: "disabled sync needs no credentials", env: map[string]string{"KEY_1": "", "SYNC_ENABLED": "false"}},
		{name: "duplicated brand", replace: [2]string{"brand_id: 2", "brand_id: 1"},
			wantErr: "brand_id 1 is missing or duplicated"},
		{name: "backoff max below initial", replace: [2]string{"backoff_max: 1h", "backoff_max: 1s"},
			wantErr: "sync.backoff_max must be >= sync.backoff_initial"},
		{name: "lock check slower than session timeout", replace: [2]string{"lock_check_interval: 5s", "lock_check_interval: 5m"},
			wantErr: "sync.lock_session_timeout must exceed sync.job_timeout + sync.lock_check_interval"},
		{name: "lock session shorter than a job", replace: [2]string{"lock_session_timeout: 300s", "lock_session_timeout: 120s"},
			wantErr: "sync.lock_session_timeout must exceed sync.job_timeout + sync.lock_check_interval"},
		{name: "lock session just outlives a job", replace: [2]string{"lock_session_timeout: 300s", "lock_session_timeout: 126s"},
			wantBrands: []uint32{1}},
		{name: "no workers", replace: [2]string{"workers_per_brand: 4", "workers_per_brand: 0"},
			wantErr: "sync.batch_per_brand and sync.workers_per_brand must be at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := validYAML + syncYAML
			if tt.replace[0] != "" {
				raw = strings.Replace(raw, tt.replace[0], tt.replace[1], 1)
			}
			env := map[string]string{"KEY_1": "k1"}
			for k, v := range tt.env {
				env[k] = v
			}
			cfg, err := Parse([]byte(raw), mapLookup(env))
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if !cfg.Sync.Enabled {
				return
			}
			var ids []uint32
			for _, b := range cfg.Acme.ConfiguredBrands() {
				ids = append(ids, b.BrandID)
			}
			assert.Equal(t, tt.wantBrands, ids)
		})
	}
}
