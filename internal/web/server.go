// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web serves the browser UI: server-rendered HTML with
// html/template (docs/adr/0001-web-frontend.md). Every handler that reads or
// changes an owned object checks role and ownership on the server
// (AGENTS.md §5).
package web

import (
	"context"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/carddav"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/regwin"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
	"github.com/openDECTcell/opendectcell-controller/internal/transfer"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Mailer sends the controller's mail; *mailer.Mailer is the real one.
type Mailer interface {
	Enabled() bool
	Send(ctx context.Context, to []string, subject, body string) error
}

// mailOn reports whether mail can be sent.
func (s *Server) mailOn() bool { return s.Mailer != nil && s.Mailer.Enabled() }

// Server is the web UI.
type Server struct {
	Pool           *pgxpool.Pool
	Auth           *auth.Store
	OIDC           *auth.OIDC // nil when OIDC is not configured
	OIDCName       string     // login button label
	LocalLogin     bool
	BaseURL        *url.URL
	SecureCookies  bool
	TrustedProxies []netip.Prefix
	proxyWarn      sync.Once
	Log            *slog.Logger
	Version        string
	Cells          *cell.Store // nil: no cell pages
	CellLink       CellLink
	SIPAccounts    *sipaccount.Store // nil: no SIP pages
	Contacts       *contacts.Store   // nil: no contact API
	Books          *carddav.Store    // nil: no address books
	Transfer       *transfer.Store   // nil: no setup export and import
	SIP            SIPStatus
	Registrations  *regwin.Registry // nil: no registration API
	// HandsetChanged, if set, is told when a handset's name or device profile
	// was changed, so the DECT stacks that hold it follow.
	HandsetChanged func(id uuid.UUID)
	// HandsetDeregister, if set, asks the DECT cells to deregister the handset.
	HandsetDeregister func(id uuid.UUID, force bool)
	Settings          *settings.Store // nil: the default instance name, no settings pages
	Stats             *stats.Store    // nil: no statistics pages
	Handsets          *handset.Store  // nil: no handset pages
	Orgs              *org.Store      // nil: no organization pages
	LogCtl            *logctl.Controller
	Mailer            Mailer // nil: no mail
	SMTPHost          string
	StartedAt         time.Time
	MetricsToken      string // empty: no /metrics

	brands brandCache
	tmpl   *template.Template
	mux    *http.ServeMux
}

// Handler builds the server's handler.
func (s *Server) Handler() (http.Handler, error) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	t, err := template.New("").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"ms":      func(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + " ms" },
		"mask":    logctl.MaskNumber,
		"dur": func(d time.Duration) string {
			return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
		},
		"f1":         func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		"icon":       s.iconFunc(),
		"callsChart": callsChart, "hms": hms, "hours": hours, "pct": pct,
		"deref": func(b *bool) bool { return b != nil && *b },
		"ipei": func(h *handset.Handset) string {
			t, _, _ := h.IPEI()
			return t
		},
		"signalWord":   handset.SignalWord,
		"endReason":    endReasonText,
		"profileLabel": func(id string) string { return profiles.Default.Label(id) },
		"hexb":         func(b []byte) string { return strings.ToUpper(hex.EncodeToString(b)) },
		"tpui":         func(t int) string { return fmt.Sprintf("%05X", t) },
		"emc": func(h *handset.Handset) string {
			if _, e, ok := h.IPEI(); ok {
				return fmt.Sprintf("%d (0x%04X)", e, e)
			}
			return ""
		},
		"deref2": func(n *int) int {
			if n == nil {
				return 0
			}
			return *n
		},
		"atoi": func(s string) int { n, _ := strconv.Atoi(s); return n },
		"join": strings.Join,
		"has":  slices.Contains[[]string, string],
		"list": func(xs ...string) []string { return xs },
		"fieldErr": func(m map[string]string, k string) string {
			if m == nil {
				return ""
			}
			return m[k]
		},
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	s.mux = http.NewServeMux()
	s.routes()

	cop := http.NewCrossOriginProtection()
	if s.BaseURL != nil {
		if err := cop.AddTrustedOrigin(s.BaseURL.Scheme + "://" + s.BaseURL.Host); err != nil {
			return nil, err
		}
	}
	var h http.Handler = s.mux
	h = s.withSession(h)
	h = cop.Handler(h)
	h = s.withClientAddr(h)
	h = s.securityHeaders(h)
	h = withRequest(h)
	h = s.withRecover(h)
	return h, nil
}

func (s *Server) routes() {
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /metrics", s.metrics)
	s.mux.HandleFunc("GET /branding/logo", s.logo)
	s.mux.HandleFunc("GET /favicon.ico", s.favicon)

	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.loginPost)
	s.mux.HandleFunc("GET /register", s.registerPage)
	s.mux.HandleFunc("POST /register", s.registerPost)
	s.mux.HandleFunc("GET /register/verify", s.verifyPage)
	s.mux.HandleFunc("POST /register/verify", s.verifyPost)
	s.mux.HandleFunc("POST /register/resend", s.resendPost)
	s.mux.HandleFunc("GET /forgot", s.forgotPage)
	s.mux.HandleFunc("POST /forgot", s.forgotPost)
	s.mux.HandleFunc("GET /reset", s.resetPage)
	s.mux.HandleFunc("POST /reset", s.resetPost)
	s.mux.HandleFunc("POST /login/2fa", s.login2FA)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /auth/oidc/start", s.oidcStart)
	s.mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)

	s.mux.Handle("GET /{$}", s.requireUser(s.home))
	s.mux.Handle("GET /account", s.requireUser(s.accountPage))
	s.mux.Handle("POST /account/password", s.requireUser(s.accountPassword))
	s.mux.Handle("GET /account/2fa", s.requireUser(s.twoFAPage))
	s.mux.Handle("POST /account/2fa/start", s.requireUser(s.twoFAStart))
	s.mux.Handle("POST /account/2fa/confirm", s.requireUser(s.twoFAConfirm))
	s.mux.Handle("POST /account/2fa/disable", s.requireUser(s.twoFADisable))
	s.accountDataRoutes()
	s.setupRoutes()
	s.mux.Handle("POST /account/2fa/devices/forget", s.requireUser(s.twoFAForgetDevices))
	s.mux.Handle("POST /account/2fa/later", s.requireUser(s.twoFALater))
	s.mux.Handle("GET /account/sessions", s.requireUser(s.sessionsPage))
	s.mux.Handle("POST /account/sessions/end", s.requireUser(s.sessionEnd))
	s.mux.Handle("POST /account/sessions/end-others", s.requireUser(s.sessionEndOthers))

	s.mux.Handle("GET /admin/users", s.requireAdmin(s.adminUsers))
	s.mux.Handle("GET /admin/audit", s.requireAdmin(s.adminAudit))
	s.mux.Handle("GET /admin/calls", s.requireAdmin(s.adminCalls))
	s.settingsRoutes()
	s.handsetRoutes()
	s.publicMapRoutes()
	s.stubRoutes()
	s.userCallRoutes()
	s.stationRoutes()
	s.orgRoutes()
	s.statsRoutes()
	s.loggingRoutes()
	s.adminRoutes()
	s.notificationRoutes()
	s.cellRoutes()
	s.identityRoutes()
	s.sipRoutes()
	s.userRoutes()
	s.apiRoutes()
	s.contactRoutes()
	s.registrationRoutes()
	s.contactPages()
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		http.Error(w, "database unavailable\n", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok\n"))
}

// middleware

// maxRequestBody caps every request body the server reads.
const maxRequestBody = 1 << 20

func (s *Server) securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		img := "'self' data:"
		if _, origin := s.mapConfig(r.Context()); origin != "" {
			img += " " + origin // the admin's tile server, the only outside host a page may load from
		}
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src "+img+"; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		if s.SecureCookies { // the site is served over https: browsers keep to it for a year
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		// no request body is larger than this (the logo upload, the biggest, is 320 KiB); handlers that expect less cap it themselves
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) withRecover(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

type addrKey struct{}

// clientAddr is the TCP peer, or, if the peer is a trusted proxy, the
// right-most address in X-Forwarded-For that is not itself a trusted proxy.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !s.trusted(peer) {
		// A proxy in front of us that is not listed makes every client look like the
		// proxy, so they would share one login rate limit: say so, once.
		if len(s.TrustedProxies) == 0 && r.Header.Get("X-Forwarded-For") != "" {
			s.proxyWarn.Do(func() {
				s.Log.Warn("a request carries X-Forwarded-For but http.trusted_proxies is empty: behind a reverse proxy every client shares the proxy's address and one login rate limit; list the proxy")
			})
		}
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !s.trusted(a) {
			return a
		}
		peer = a
	}
	return peer
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) withClientAddr(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := s.clientAddr(r)
		ctx := context.WithValue(r.Context(), addrKey{}, a)
		ctx = audit.WithActor(ctx, audit.Actor{Name: "anonymous", Addr: a})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func addrFrom(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(addrKey{}).(netip.Addr)
	return a
}

func (s *Server) cookieName() string {
	if s.SecureCookies {
		// __Host- binds the cookie to this origin: Secure, Path=/, no Domain.
		return "__Host-opendectcell_session"
	}
	return "opendectcell_session"
}

type sessionKey struct{}

func (s *Server) withSession(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cookieName())
		if err == nil && c.Value != "" {
			sess, err := s.Auth.LookupSession(r.Context(), c.Value)
			switch {
			case err == nil:
				user := sess.User
				if s.adminNeeds2FA(r, user) {
					// not an administrator at all until they have set up two-factor login: every route that checks
					// the role, not only the admin pages, then refuses them
					plain := *user
					plain.Role, plain.Admin2FAPending = auth.RoleUser, true
					user = &plain
				}
				ctx := auth.WithUser(r.Context(), user)
				a := user.Actor()
				a.Addr = addrFrom(ctx)
				ctx = audit.WithActor(ctx, a)
				ctx = context.WithValue(ctx, sessionKey{}, sess)
				r = r.WithContext(ctx)
			case errors.Is(err, auth.ErrNoSession):
				s.clearCookie(w)
			default:
				s.Log.Error("session lookup", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func sessionFrom(ctx context.Context) *auth.Session {
	sess, _ := ctx.Value(sessionKey{}).(*auth.Session)
	return sess
}

func (s *Server) setCookie(w http.ResponseWriter, tok string) {
	// Secure is off only when the base URL is plain http (development).
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name: s.cookieName(), Value: tok, Path: "/",
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: int(s.Auth.SessionMaxAge / time.Second),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, //nolint:gosec // G124: as setCookie
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// requireUser sends anonymous requests to the login page.
func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, *auth.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := auth.UserFrom(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, u)
	})
}

// requireAdmin answers 404 to non-admins, so admin pages are not discoverable.
func (s *Server) requireAdmin(h func(http.ResponseWriter, *http.Request, *auth.User)) http.Handler {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request, u *auth.User) {
		if u.Admin2FAPending {
			http.Redirect(w, r, "/account/2fa?need=1", http.StatusSeeOther)
			return
		}
		if auth.RequireAdmin(u) != nil {
			s.notFound(w, r, u)
			return
		}
		h(w, r, u)
	})
}
