// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads the controller configuration from an optional YAML
// file and from environment variables, which take precedence.
//
// Every setting has an environment variable named OPENDECTCELL_<NAME>. Secrets
// can instead be read from a file named by OPENDECTCELL_<NAME>_FILE (for Docker
// secrets); setting both is an error. See docs/configuration.md.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the complete controller configuration.
type Config struct {
	HTTP     HTTP     `yaml:"http"`
	Database Database `yaml:"database"`
	Secrets  Secrets  `yaml:"secrets"`
	Log      Log      `yaml:"log"`
	Auth     Auth     `yaml:"auth"`
	Cells    Cells    `yaml:"cells"`
}

type HTTP struct {
	// Listen is the address of the web UI and API, e.g. ":8080".
	Listen string `yaml:"listen"`
	// BaseURL is the external URL users reach the controller at. It sets
	// cookie Secure flags and the OIDC redirect URL.
	BaseURL string `yaml:"base_url"`
}

type Database struct {
	// URL is a PostgreSQL connection URL. Secret: it may contain a password.
	URL string `yaml:"url"`
	// MaxConns bounds the connection pool.
	MaxConns int32 `yaml:"max_conns"`
}

type Secrets struct {
	// MasterKey is the base64 key-encryption key (docs/threat-model.md §4.1).
	MasterKey string `yaml:"master_key"`
	// OldMasterKey is set only while rotating the master key.
	OldMasterKey string `yaml:"old_master_key"`
}

type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
}

type Auth struct {
	Local          LocalAuth     `yaml:"local"`
	OIDC           OIDC          `yaml:"oidc"`
	SessionIdle    time.Duration `yaml:"session_idle"`
	SessionMaxAge  time.Duration `yaml:"session_max_age"`
	LoginRateLimit int           `yaml:"login_rate_limit"` // failed attempts per user and per client address per 15 min
}

type LocalAuth struct {
	Enabled bool `yaml:"enabled"`
	// Argon2id parameters (docs/threat-model.md §5).
	ArgonMemoryKiB uint32 `yaml:"argon_memory_kib"`
	ArgonTime      uint32 `yaml:"argon_time"`
	ArgonThreads   uint8  `yaml:"argon_threads"`
}

type OIDC struct {
	// Enabled when Issuer is set.
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
	// GroupsClaim names the ID-token claim that lists the user's groups.
	GroupsClaim string `yaml:"groups_claim"`
	// AdminGroups grant the admin role.
	AdminGroups []string `yaml:"admin_groups"`
	// UserGroups grant the user role. Empty means every user the IdP
	// authenticates for this client may log in as a user.
	UserGroups []string `yaml:"user_groups"`
	// ProviderName is shown on the login button.
	ProviderName string `yaml:"provider_name"`
}

// Enabled reports whether OIDC login is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

type Cells struct {
	// Listen is the address of the cell control port (docs/cell-protocol.md §1).
	Listen string `yaml:"listen"`
}

// Default returns the configuration used for anything not set.
func Default() Config {
	return Config{
		HTTP:     HTTP{Listen: ":8080"},
		Database: Database{MaxConns: 10},
		Log:      Log{Level: "info", Format: "text"},
		Auth: Auth{
			Local:          LocalAuth{Enabled: true, ArgonMemoryKiB: 64 * 1024, ArgonTime: 3, ArgonThreads: 2},
			OIDC:           OIDC{Scopes: []string{"openid", "profile", "email"}, GroupsClaim: "groups", ProviderName: "Single sign-on"},
			SessionIdle:    12 * time.Hour,
			SessionMaxAge:  7 * 24 * time.Hour,
			LoginRateLimit: 10,
		},
		Cells: Cells{Listen: ":7300"},
	}
}

// Load reads the file at path (if non-empty), then applies the environment.
// getenv is os.Getenv in production; tests pass their own.
func Load(path string, getenv func(string) string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	envErr := applyEnv(&c, getenv)
	return c, errors.Join(envErr, c.Validate())
}

type envVar struct {
	name   string
	secret bool
	set    func(string) error
}

func str(p *string) func(string) error { return func(v string) error { *p = v; return nil } }

func list(p *[]string) func(string) error {
	return func(v string) error {
		*p = nil
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				*p = append(*p, s)
			}
		}
		return nil
	}
}

func boolean(p *bool) func(string) error {
	return func(v string) error {
		b, err := strconv.ParseBool(v)
		*p = b
		return err
	}
}

func dur(p *time.Duration) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		*p = d
		return err
	}
}

func uintN[T ~uint8 | ~uint32 | ~int32 | ~int](p *T, bits int) func(string) error {
	return func(v string) error {
		n, err := strconv.ParseUint(v, 10, bits)
		*p = T(n)
		return err
	}
}

func vars(c *Config) []envVar {
	return []envVar{
		{"HTTP_LISTEN", false, str(&c.HTTP.Listen)},
		{"HTTP_BASE_URL", false, str(&c.HTTP.BaseURL)},
		{"DATABASE_URL", true, str(&c.Database.URL)},
		{"DATABASE_MAX_CONNS", false, uintN(&c.Database.MaxConns, 31)},
		{"MASTER_KEY", true, str(&c.Secrets.MasterKey)},
		{"MASTER_KEY_OLD", true, str(&c.Secrets.OldMasterKey)},
		{"LOG_LEVEL", false, str(&c.Log.Level)},
		{"LOG_FORMAT", false, str(&c.Log.Format)},
		{"AUTH_LOCAL_ENABLED", false, boolean(&c.Auth.Local.Enabled)},
		{"AUTH_ARGON_MEMORY_KIB", false, uintN(&c.Auth.Local.ArgonMemoryKiB, 32)},
		{"AUTH_ARGON_TIME", false, uintN(&c.Auth.Local.ArgonTime, 32)},
		{"AUTH_ARGON_THREADS", false, uintN(&c.Auth.Local.ArgonThreads, 8)},
		{"AUTH_SESSION_IDLE", false, dur(&c.Auth.SessionIdle)},
		{"AUTH_SESSION_MAX_AGE", false, dur(&c.Auth.SessionMaxAge)},
		{"AUTH_LOGIN_RATE_LIMIT", false, uintN(&c.Auth.LoginRateLimit, 31)},
		{"OIDC_ISSUER", false, str(&c.Auth.OIDC.Issuer)},
		{"OIDC_CLIENT_ID", false, str(&c.Auth.OIDC.ClientID)},
		{"OIDC_CLIENT_SECRET", true, str(&c.Auth.OIDC.ClientSecret)},
		{"OIDC_SCOPES", false, list(&c.Auth.OIDC.Scopes)},
		{"OIDC_GROUPS_CLAIM", false, str(&c.Auth.OIDC.GroupsClaim)},
		{"OIDC_ADMIN_GROUPS", false, list(&c.Auth.OIDC.AdminGroups)},
		{"OIDC_USER_GROUPS", false, list(&c.Auth.OIDC.UserGroups)},
		{"OIDC_PROVIDER_NAME", false, str(&c.Auth.OIDC.ProviderName)},
		{"CELLS_LISTEN", false, str(&c.Cells.Listen)},
	}
}

// EnvNames lists every environment variable the controller reads, for
// documentation and tests.
func EnvNames() []string {
	var c Config
	var out []string
	for _, v := range vars(&c) {
		out = append(out, "OPENDECTCELL_"+v.name)
		if v.secret {
			out = append(out, "OPENDECTCELL_"+v.name+"_FILE")
		}
	}
	return out
}

func applyEnv(c *Config, getenv func(string) string) error {
	var errs []error
	for _, v := range vars(c) {
		name := "OPENDECTCELL_" + v.name
		val := getenv(name)
		if v.secret {
			if f := getenv(name + "_FILE"); f != "" {
				if val != "" {
					errs = append(errs, fmt.Errorf("config: set %s or %s_FILE, not both", name, name))
					continue
				}
				b, err := os.ReadFile(f)
				if err != nil {
					errs = append(errs, fmt.Errorf("config: %s_FILE: %w", name, err))
					continue
				}
				val = strings.TrimRight(string(b), "\r\n")
				if val == "" {
					errs = append(errs, fmt.Errorf("config: %s_FILE: file is empty", name))
					continue
				}
			}
		}
		if val == "" {
			continue
		}
		if err := v.set(val); err != nil {
			// Never echo a secret value back in an error.
			if v.secret {
				errs = append(errs, fmt.Errorf("config: %s: invalid value", name))
			} else {
				errs = append(errs, fmt.Errorf("config: %s=%q: %v", name, val, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Validate checks the configuration and reports every problem at once.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("config: "+format, a...)) }

	if c.Database.URL == "" {
		add("database URL is required (OPENDECTCELL_DATABASE_URL or OPENDECTCELL_DATABASE_URL_FILE)")
	}
	if c.Secrets.MasterKey == "" {
		add("master key is required (OPENDECTCELL_MASTER_KEY_FILE or OPENDECTCELL_MASTER_KEY); generate one with `opendectcell-controller keygen`")
	}
	if c.HTTP.BaseURL == "" {
		add("base URL is required (OPENDECTCELL_HTTP_BASE_URL), e.g. https://dect.example.org")
	} else if u, err := url.Parse(c.HTTP.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("base URL %q must be an absolute http(s) URL", c.HTTP.BaseURL)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log level %q must be debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log format %q must be text or json", c.Log.Format)
	}
	if !c.Auth.Local.Enabled && !c.Auth.OIDC.Enabled() {
		add("no login method: enable local accounts or configure OIDC")
	}
	if c.Auth.Local.ArgonMemoryKiB < 19*1024 || c.Auth.Local.ArgonTime < 1 || c.Auth.Local.ArgonThreads < 1 {
		add("Argon2id parameters below the minimum (19 MiB, 1 pass, 1 thread)")
	}
	if c.Auth.SessionIdle <= 0 || c.Auth.SessionMaxAge < c.Auth.SessionIdle {
		add("session lifetimes must satisfy 0 < session_idle <= session_max_age")
	}
	if c.Auth.LoginRateLimit < 1 {
		add("login rate limit must be at least 1")
	}
	if o := c.Auth.OIDC; o.Enabled() {
		if u, err := url.Parse(o.Issuer); err != nil || u.Scheme != "https" && !isLoopback(u.Hostname()) {
			add("OIDC issuer %q must be an https URL", o.Issuer)
		}
		if o.ClientID == "" {
			add("OIDC client ID is required when an issuer is set")
		}
		if o.GroupsClaim == "" && (len(o.AdminGroups) > 0 || len(o.UserGroups) > 0) {
			add("OIDC groups claim is required when group mappings are set")
		}
		if !contains(o.Scopes, "openid") {
			add("OIDC scopes must include openid")
		}
	}
	if c.Database.MaxConns < 1 {
		add("database max_conns must be at least 1")
	}
	return errors.Join(errs...)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// SecureCookies reports whether cookies get the Secure flag.
func (c Config) SecureCookies() bool { return strings.HasPrefix(c.HTTP.BaseURL, "https://") }
