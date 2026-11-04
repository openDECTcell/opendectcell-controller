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
	"net"
	"net/mail"
	"net/netip"
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
	SIP      SIP      `yaml:"sip"`
	SMTP     SMTP     `yaml:"smtp"`
	CardDAV  CardDAV  `yaml:"carddav"`
}

// CardDAV is how the controller fetches the address books its users link.
type CardDAV struct {
	// AllowPrivate lets the controller connect to addresses on private networks (10/8, 172.16/12, 192.168/16,
	// fc00::/7), for an address book on the operator's own network. Off by default: on a controller that other
	// people use, a user could otherwise make it probe the network around it. Loopback and link-local
	// addresses are never allowed.
	AllowPrivate bool `yaml:"allow_private"`
}

// SMTP is the outgoing mail server, for notifications. Without a host the
// controller sends no mail and the pages say so.
type SMTP struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`     // default 587
	User     string `yaml:"user"`     // empty: no authentication
	Password string `yaml:"password"` // secret
	From     string `yaml:"from"`     // sender address
	// Security is starttls (default), tls (implicit, port 465) or none.
	Security string `yaml:"security"`
}

// Enabled reports whether mail can be sent.
func (m SMTP) Enabled() bool { return m.Host != "" }

// SIP configures the SIP user agents' transports and media (docs/sip-ua.md).
type SIP struct {
	UDPListen string `yaml:"udp_listen"` // shared by every account
	TCPListen string `yaml:"tcp_listen"` // "" disables incoming TCP
	RTPPorts  string `yaml:"rtp_ports"`  // "min-max"
	// MediaIP, if set, is advertised in SDP instead of the local address
	// (a static public address, voip-requirements.md §2).
	MediaIP string `yaml:"media_ip"`
}

// RTPRange parses RTPPorts; Validate has checked it.
func (s SIP) RTPRange() (lo, hi int) {
	a, b, _ := strings.Cut(s.RTPPorts, "-")
	lo, _ = strconv.Atoi(strings.TrimSpace(a))
	hi, _ = strconv.Atoi(strings.TrimSpace(b))
	return lo, hi
}

// HTTP configures the web UI and API listener (docs/configuration.md).
type HTTP struct {
	// Listen is the address of the web UI and API, e.g. ":8080".
	Listen string `yaml:"listen"`
	// BaseURL is the external URL users reach the controller at. It sets
	// cookie Secure flags and the OIDC redirect URL.
	BaseURL string `yaml:"base_url"`
	// TrustedProxies are the reverse proxies (CIDRs) whose X-Forwarded-For
	// header is believed. Empty: the client address is the TCP peer.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// MetricsToken turns on /metrics (Prometheus) for a client that sends it
	// as a bearer token. Secret. Empty: no endpoint.
	MetricsToken string `yaml:"metrics_token"`
}

// Database configures the PostgreSQL connection pool.
type Database struct {
	// URL is a PostgreSQL connection URL. Secret: it may contain a password.
	URL string `yaml:"url"`
	// MaxConns bounds the connection pool.
	MaxConns int32 `yaml:"max_conns"`
	// AdminURL is the connection URL of the role that owns the schema. Only `migrate` (and `serve` without
	// --no-migrate) uses it: it creates the role in URL, applies the migrations and grants that role the
	// table rights. Leave it out of the controller's own environment (docs/deployment.md, "Database roles").
	AdminURL string `yaml:"admin_url"`
}

// Secrets holds the keys that protect secrets in the database: DECT keys, SIP
// credentials and two-factor secrets (docs/threat-model.md §4.1). The values
// come from the file or the environment, never from the repository.
type Secrets struct {
	// MasterKey is the base64 key-encryption key (docs/threat-model.md §4.1).
	MasterKey string `yaml:"master_key"`
	// OldMasterKey is set only while rotating the master key.
	OldMasterKey string `yaml:"old_master_key"`
}

// Log sets the default log level and output format; logctl can raise the level of
// single components at run time.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
}

// Auth configures login: local passwords, OpenID Connect and the session lifetimes.
// SessionIdle and SessionMaxAge are the idle and the absolute lifetime of a session.
type Auth struct {
	Local          LocalAuth     `yaml:"local"`
	OIDC           OIDC          `yaml:"oidc"`
	SessionIdle    time.Duration `yaml:"session_idle"`
	SessionMaxAge  time.Duration `yaml:"session_max_age"`
	LoginRateLimit int           `yaml:"login_rate_limit"` // failed attempts per user and per client address per 15 min
}

// LocalAuth configures password login with the controller's own user table.
type LocalAuth struct {
	Enabled bool `yaml:"enabled"`
	// Argon2id parameters (docs/threat-model.md §5).
	ArgonMemoryKiB uint32 `yaml:"argon_memory_kib"`
	ArgonTime      uint32 `yaml:"argon_time"`
	ArgonThreads   uint8  `yaml:"argon_threads"`
}

// OIDC configures login through an OpenID Connect provider (docs/authentik.md).
// It is off while Issuer is empty.
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

// Cells configures the cell control port and the media ports the cells send to
// (docs/cell-protocol.md).
type Cells struct {
	// Listen is the address of the cell control port (docs/cell-protocol.md §1).
	Listen string `yaml:"listen"`
	// PublicAddress is host:port as cells reach the control port; it goes
	// into enrollment strings. Default: the base URL's host and the
	// listen port.
	PublicAddress string `yaml:"public_address"`
	// HandsetPool: a handset that enrols is held by every cell under a
	// TPUI of its own (docs/roaming.md), not by one cell's chip. Off: it
	// is a chip-slot handset of the cell it enrolled on, as before.
	HandsetPool bool `yaml:"handset_pool"`
	// MediaIP is the address cells send a call's speech to (docs/cell-protocol.md
	// §8), MediaPorts the UDP range the controller listens on for it ("min-max",
	// published like the SIP RTP range). Default MediaIP: the host of
	// PublicAddress, when that is an IP address. "auto": each cell is sent to the address it
	// connected to, which is right with no address translation in between (the host network) and
	// when cells reach the controller by different addresses (LAN and public IPv6).
	MediaIP    string `yaml:"media_ip"`
	MediaPorts string `yaml:"media_ports"`
	// StackWorkers is how many worker processes run the cells' DECT stacks
	// (docs/adr/0003-stack-isolation.md): a crash or a hang in the C library
	// ends one worker, not the controller. 0: the stacks run inside the
	// controller process.
	StackWorkers int32 `yaml:"stack_workers"`
	// RequireDECTCore makes `serve` refuse to start when the binary was built without libopendectcell, so a
	// controller that would drop every cell's mails cannot be mistaken for a working one.
	RequireDECTCore bool `yaml:"require_dect_core"`
}

// MediaRange parses MediaPorts; Validate has checked it.
func (c Cells) MediaRange() (lo, hi int) {
	a, b, _ := strings.Cut(c.MediaPorts, "-")
	lo, _ = strconv.Atoi(strings.TrimSpace(a))
	hi, _ = strconv.Atoi(strings.TrimSpace(b))
	return lo, hi
}

// CellMediaIP returns MediaIP, or the host of the public address if that is an
// IP address; invalid when neither is.
func (c Config) CellMediaIP() netip.Addr {
	if c.CellMediaAuto() {
		return netip.Addr{}
	}
	if c.Cells.MediaIP != "" {
		a, _ := netip.ParseAddr(c.Cells.MediaIP)
		return a
	}
	h, _, err := net.SplitHostPort(c.CellPublicAddress())
	if err != nil {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(h)
	return a
}

// CellMediaAuto says the cells' speech goes to the address each cell connected to.
func (c Config) CellMediaAuto() bool { return strings.EqualFold(c.Cells.MediaIP, "auto") }

// CellPublicAddress returns PublicAddress or its default.
func (c Config) CellPublicAddress() string {
	if c.Cells.PublicAddress != "" {
		return c.Cells.PublicAddress
	}
	host := "localhost"
	if u, err := url.Parse(c.HTTP.BaseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	_, port, err := net.SplitHostPort(c.Cells.Listen)
	if err != nil || port == "" {
		port = "7300"
	}
	return net.JoinHostPort(host, port)
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
		Cells: Cells{Listen: ":7300", HandsetPool: true, MediaPorts: "41100-41199"},
		SIP:   SIP{UDPListen: ":5060", TCPListen: ":5060", RTPPorts: "41000-41999"},
		SMTP:  SMTP{Port: "587", Security: "starttls"},
	}
}

// Load reads the file at path (if non-empty), then applies the environment.
// getenv is os.Getenv in production; tests pass their own.
func Load(path string, getenv func(string) string) (Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
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
		{"HTTP_TRUSTED_PROXIES", false, list(&c.HTTP.TrustedProxies)},
		{"HTTP_METRICS_TOKEN", true, str(&c.HTTP.MetricsToken)},
		{"DATABASE_URL", true, str(&c.Database.URL)},
		{"DATABASE_ADMIN_URL", true, str(&c.Database.AdminURL)},
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
		{"CELLS_PUBLIC_ADDRESS", false, str(&c.Cells.PublicAddress)},
		{"CELLS_HANDSET_POOL", false, boolean(&c.Cells.HandsetPool)},
		{"CELLS_STACK_WORKERS", false, uintN(&c.Cells.StackWorkers, 8)},
		{"CELLS_REQUIRE_DECT_CORE", false, boolean(&c.Cells.RequireDECTCore)},
		{"CELLS_MEDIA_IP", false, str(&c.Cells.MediaIP)},
		{"CELLS_MEDIA_PORTS", false, str(&c.Cells.MediaPorts)},
		{"SIP_UDP_LISTEN", false, str(&c.SIP.UDPListen)},
		{"SIP_TCP_LISTEN", false, str(&c.SIP.TCPListen)},
		{"SIP_RTP_PORTS", false, str(&c.SIP.RTPPorts)},
		{"SIP_MEDIA_IP", false, str(&c.SIP.MediaIP)},
		{"CARDDAV_ALLOW_PRIVATE", false, boolean(&c.CardDAV.AllowPrivate)},
		{"SMTP_HOST", false, str(&c.SMTP.Host)},
		{"SMTP_PORT", false, str(&c.SMTP.Port)},
		{"SMTP_USER", false, str(&c.SMTP.User)},
		{"SMTP_PASSWORD", true, str(&c.SMTP.Password)},
		{"SMTP_FROM", false, str(&c.SMTP.From)},
		{"SMTP_SECURITY", false, str(&c.SMTP.Security)},
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
				b, err := os.ReadFile(f) //nolint:gosec // G304: the operator names the file
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
				errs = append(errs, fmt.Errorf("config: %s=%q: %w", name, val, err))
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
	if t := c.HTTP.MetricsToken; t != "" && len(t) < 32 {
		add("the metrics token is too short (%d characters): it is the only thing that guards /metrics, use at least 32 (`opendectcell-controller keygen` makes one)", len(t))
	}
	for _, p := range c.HTTP.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			add("trusted proxy %q is not a CIDR prefix (e.g. 172.16.0.0/12)", p)
		}
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
	if c.Cells.PublicAddress != "" {
		if h, p, err := net.SplitHostPort(c.Cells.PublicAddress); err != nil || h == "" || p == "" {
			add("cells public address %q must be host:port", c.Cells.PublicAddress)
		}
	}
	if c.SIP.UDPListen == "" {
		add("SIP UDP listen address is required (OPENDECTCELL_SIP_UDP_LISTEN, e.g. :5060)")
	}
	if lo, hi := c.SIP.RTPRange(); lo < 1024 || hi > 65535 || hi-lo < 3 {
		add("SIP RTP ports %q must be a range like 41000-41999 above 1023", c.SIP.RTPPorts)
	}
	if lo, hi := c.Cells.MediaRange(); lo < 1024 || hi > 65535 || hi-lo < 3 {
		add("cells media ports %q must be a range like 41100-41199 above 1023", c.Cells.MediaPorts)
	}
	if c.Cells.MediaIP != "" && !c.CellMediaAuto() {
		if _, err := netip.ParseAddr(c.Cells.MediaIP); err != nil {
			add("cells media IP %q is not an IP address (or \"auto\")", c.Cells.MediaIP)
		}
	}
	if c.SIP.MediaIP != "" {
		if _, err := netip.ParseAddr(c.SIP.MediaIP); err != nil {
			add("SIP media IP %q is not an IP address", c.SIP.MediaIP)
		}
	}
	if c.SMTP.Enabled() {
		if _, err := mail.ParseAddress(c.SMTP.From); err != nil {
			add("SMTP from address %q is required and must be an e-mail address when an SMTP host is set", c.SMTP.From)
		}
		switch c.SMTP.Security {
		case "starttls", "tls", "none":
		default:
			add("SMTP security %q must be starttls, tls or none", c.SMTP.Security)
		}
		if n, err := strconv.Atoi(c.SMTP.Port); err != nil || n < 1 || n > 65535 {
			add("SMTP port %q is not a port number", c.SMTP.Port)
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

// TrustedProxyPrefixes returns TrustedProxies parsed; Validate has checked them.
func (c Config) TrustedProxyPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range c.HTTP.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pf.Masked())
		}
	}
	return out
}

// SecureCookies reports whether cookies get the Secure flag.
func (c Config) SecureCookies() bool { return strings.HasPrefix(c.HTTP.BaseURL, "https://") }
