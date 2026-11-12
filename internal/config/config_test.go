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
`,
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
