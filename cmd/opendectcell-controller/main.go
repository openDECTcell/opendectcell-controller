// SPDX-License-Identifier: AGPL-3.0-or-later

// Command opendectcell-controller is the DECT↔SIP controller for openDECTcell cells.
//
//	opendectcell-controller serve [--no-migrate]
//	opendectcell-controller migrate up | down [n] | status
//	opendectcell-controller keygen
//	opendectcell-controller rekey
//	opendectcell-controller rotate-dek
//	opendectcell-controller user create --username NAME --role admin|user [--display-name N] [--email E] [--may-add-cells]
//	opendectcell-controller user set-password --username NAME
//	opendectcell-controller version
//	opendectcell-controller healthcheck [--url URL]
//	opendectcell-controller init-secrets --dir DIR [--db-host H] [--db-name N] [--db-user U]
//
// Every command except keygen and version reads the configuration from
// --config FILE (or OPENDECTCELL_CONFIG) and the environment (docs/configuration.md).
// Passwords are read from standard input, never from the command line.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the zone database, so TZ works in an image without one

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/bridge"
	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/carddav"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/config"
	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	dectcore "github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/regwin"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/worker"
	"github.com/openDECTcell/opendectcell-controller/internal/handset"
	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
	"github.com/openDECTcell/opendectcell-controller/internal/mailer"
	"github.com/openDECTcell/opendectcell-controller/internal/org"
	"github.com/openDECTcell/opendectcell-controller/internal/retention"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sipaccount"
	"github.com/openDECTcell/opendectcell-controller/internal/sipevents"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
	"github.com/openDECTcell/opendectcell-controller/internal/transfer"
	"github.com/openDECTcell/opendectcell-controller/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return "dev"
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "opendectcell-controller:", err)
		os.Exit(1)
	}
}

const usage = `usage: opendectcell-controller [--config FILE] COMMAND

commands:
  serve [--no-migrate]     run the controller (applies migrations first)
  migrate up|down [n]|status
  keygen                   print a new master key
  rekey                    re-wrap data keys under a new master key
  rotate-dek               re-encrypt all secrets under a new data key
  user create --username NAME --role admin|user [--display-name N] [--email E] [--may-add-cells]
  user set-password --username NAME
  user reset-2fa --username NAME   turn a user's two-factor login off (lost app and recovery codes)
  cellworker               a DECT stack worker process (started by serve; not for hand use)
  version
  healthcheck [--url URL]  exit 0 if the controller answers /healthz (for container health checks)
  init-secrets --dir DIR [--admin-dir DIR] [--limited-role]   create a master key and database credentials if missing (compose bring-up)
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("opendectcell-controller", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", os.Getenv("OPENDECTCELL_CONFIG"), "configuration file (YAML)")
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return errors.New("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		fmt.Fprintf(stdout, "dect-core: %s\n", map[bool]string{true: "yes", false: "no"}[dectStack != nil])
		return nil
	case "keygen":
		k, err := secret.GenerateMasterKey()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, k)
		return nil
	case "healthcheck":
		return healthcheck(rest)
	case "cellworker":
		return cellWorker()
	case "init-secrets":
		return initSecrets(rest, stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}

	cfg, err := config.Load(*cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	log, logCtl := newLogger(cfg.Log, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// With the owner of the schema given, the role the controller runs as is made and the schema is brought
	// up to date before the controller connects (it may not exist yet).
	if cfg.Database.AdminURL != "" {
		switch {
		case cmd == "migrate":
			return migrateAsAdmin(ctx, cfg, rest, stdout, log)
		case cmd == "serve" && !slices.Contains(rest, "--no-migrate"):
			if err := setupAsAdmin(ctx, cfg, log); err != nil {
				return err
			}
		}
	}

	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, pool, log, logCtl, rest)
	case "migrate":
		return migrate(ctx, pool, rest, stdout)
	case "rekey", "rotate-dek":
		return keys(ctx, cmd, cfg, pool, stdout)
	case "user":
		return userCmd(ctx, cfg, pool, rest, stdin, stdout, stderr)
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// newLogger builds the logger. The configured level is the default; an admin
// can change it per component while running (internal/logctl). The handler
// underneath lets everything through, logctl decides what is written.
func newLogger(c config.Log, w io.Writer) (*slog.Logger, *logctl.Controller) {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.Level)) // validated by config.Validate
	ctl := logctl.New(lvl)
	opts := &slog.HandlerOptions{Level: logctl.LevelCalls}
	var inner slog.Handler = slog.NewTextHandler(w, opts)
	if c.Format == "json" {
		inner = slog.NewJSONHandler(w, opts)
	}
	return slog.New(ctl.Handler(inner)), ctl
}

func masterKeys(cfg config.Config) (secret.MasterKey, *secret.MasterKey, error) {
	mk, err := secret.ParseMasterKey(cfg.Secrets.MasterKey)
	if err != nil {
		return mk, nil, err
	}
	if cfg.Secrets.OldMasterKey == "" {
		return mk, nil, nil
	}
	old, err := secret.ParseMasterKey(cfg.Secrets.OldMasterKey)
	if err != nil {
		return mk, nil, fmt.Errorf("old master key: %w", err)
	}
	return mk, &old, nil
}

// adminPool opens the connection of the role that owns the schema and says which role the controller runs as.
//
// limited is false when both URLs name the same role (an installation from before the split): that role is then left as
// it is, since making it a limited role would strip the owner of the schema of its rights.
func adminPool(ctx context.Context, cfg config.Config, log *slog.Logger) (admin *pgxpool.Pool, runtimeUser, runtimePassword string, limited bool, err error) {
	runtimeUser, runtimePassword, err = db.RuntimeCredentials(cfg.Database.URL)
	if err != nil {
		return nil, "", "", false, err
	}
	adminUser, _, err := db.RuntimeCredentials(cfg.Database.AdminURL)
	if err != nil {
		return nil, "", "", false, errors.New("the database admin URL names no user")
	}
	if limited = adminUser != runtimeUser; !limited {
		log.Warn("the database URL and the admin URL name the same role: the controller runs with the rights of the schema's owner, and no limited role is made", "role", adminUser)
	}
	admin, err = db.Open(ctx, cfg.Database.AdminURL, 2)
	return admin, runtimeUser, runtimePassword, limited, err
}

// setupAsAdmin makes the runtime role, applies the migrations and gives the role its table rights, as the owner
// of the schema, and lets go of that connection.
func setupAsAdmin(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	admin, user, pw, limited, err := adminPool(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer admin.Close()
	if limited {
		if err := db.EnsureRuntimeRole(ctx, admin, user, pw); err != nil {
			return fmt.Errorf("database role %q: %w", user, err)
		}
	}
	if err := ensureSchema(ctx, admin, true, log); err != nil {
		return err
	}
	if !limited {
		return nil
	}
	return db.GrantRuntime(ctx, admin, user)
}

// migrateAsAdmin is `migrate` run as the owner of the schema: up also makes the runtime role first and grants it its
// rights afterwards.
func migrateAsAdmin(ctx context.Context, cfg config.Config, args []string, out io.Writer, log *slog.Logger) error {
	admin, user, pw, limited, err := adminPool(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer admin.Close()
	up := len(args) > 0 && args[0] == "up" && limited
	if up {
		if err := db.EnsureRuntimeRole(ctx, admin, user, pw); err != nil {
			return fmt.Errorf("database role %q: %w", user, err)
		}
	}
	if err := migrate(ctx, admin, args, out); err != nil {
		return err
	}
	if up {
		if err := db.GrantRuntime(ctx, admin, user); err != nil {
			return err
		}
		fmt.Fprintf(out, "role %s may read and write the tables\n", user)
	}
	return nil
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool, apply bool, log *slog.Logger) error {
	m, err := db.NewMigrator(pool)
	if err != nil {
		return err
	}
	if apply {
		did, err := m.Up(ctx)
		if err != nil {
			return err
		}
		if len(did) > 0 {
			log.Info("applied migrations", "versions", did)
		}
		return nil
	}
	cur, latest, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if cur != latest {
		return fmt.Errorf("database schema is at version %d, this controller needs %d: run `opendectcell-controller migrate up`", cur, latest)
	}
	return nil
}

func authStore(cfg config.Config, pool *pgxpool.Pool) *auth.Store {
	return &auth.Store{
		Pool: pool,
		Argon: auth.ArgonParams{MemoryKiB: cfg.Auth.Local.ArgonMemoryKiB, Time: cfg.Auth.Local.ArgonTime,
			Threads: cfg.Auth.Local.ArgonThreads},
		SessionIdle: cfg.Auth.SessionIdle, SessionMaxAge: cfg.Auth.SessionMaxAge,
		RateLimit: cfg.Auth.LoginRateLimit,
	}
}

func serve(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, logCtl *logctl.Controller, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	noMigrate := fs.Bool("no-migrate", false, "do not apply pending migrations; refuse to start if any are pending")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.Cells.RequireDECTCore && dectStack == nil {
		return errors.New("this binary was built without libopendectcell (-tags libopendectcell) but OPENDECTCELL_CELLS_REQUIRE_DECT_CORE is set: it would drop every cell's mails; build the image with LIBOPENDECTCELL=1")
	}
	if super, err := db.IsSuperuser(ctx, pool); err == nil && super {
		log.Warn("the controller is connected to PostgreSQL as a superuser: a flaw that lets someone run SQL then reaches the whole server; use a limited role (docs/deployment.md, \"Database roles\")")
	}
	if dectStack != nil && cfg.Cells.StackWorkers == 0 {
		log.Warn("the DECT stacks run inside the controller process: a crash in the C library ends the controller and every call; set OPENDECTCELL_CELLS_STACK_WORKERS (docs/adr/0003-stack-isolation.md)")
	}
	if err := ensureSchema(ctx, pool, !*noMigrate, log); err != nil {
		return err
	}
	// say that a controller runs (a command such as rotate-dek refuses to run beside one); two on one database is a mistake
	if release, held, err := db.HoldRunLock(ctx, pool); err != nil {
		log.Warn("the run lock could not be taken", "err", err)
	} else if !held {
		log.Warn("another controller seems to run against this database")
	} else {
		defer release()
	}
	instance := &settings.Store{Pool: pool}
	var saved logctl.State
	if ok, err := instance.Get(ctx, settings.KeyLogging, &saved); err != nil {
		log.Warn("saved log levels not read", "err", err)
	} else if ok {
		logCtl.Apply(saved)
	}
	mk, old, err := masterKeys(cfg)
	if err != nil {
		return err
	}
	kr, err := db.LoadKeyring(ctx, pool, mk, old)
	if err != nil {
		return err
	}
	if old != nil {
		log.Warn("an old master key is configured; run `opendectcell-controller rekey`, then remove it")
	}

	cells, err := cell.Open(ctx, pool, kr, cfg.CellPublicAddress())
	if err != nil {
		return err
	}
	// whether every user may enroll cells is a setting; it is read each time, so a change counts at once
	everyoneMayEnroll := func(ctx context.Context) bool {
		c, err := instance.Cells(ctx)
		return err == nil && c.EveryoneMayEnroll
	}
	cells.EveryoneMayEnroll = everyoneMayEnroll
	base, _ := url.Parse(cfg.HTTP.BaseURL)
	link := &cell.Server{Store: cells, Log: log.With("component", "cells"), Hosts: []string{base.Hostname()}}
	var windows *regwin.Registry
	var dectCore *dectcore.Core
	// Organizations and the trust setting (ADR 0007); the DECT core, once it exists, is told
	// whenever what a handset may use has changed.
	orgs := &org.Store{Pool: pool}
	// The contacts: the web pages' store, and the phonebook of the handsets; a change of either
	// gives the handsets a new Contact List (set below, once the DECT core exists).
	contactStore := &contacts.Store{Pool: pool}
	// the address books users link (CardDAV), copied into contacts by a sync that runs below
	books := &carddav.Store{Pool: pool, Client: &carddav.Client{AllowPrivate: cfg.CardDAV.AllowPrivate}}
	// The calls between the handsets and SIP (internal/bridge): the SIP side
	// is set below, once it exists.
	calls := &bridge.Manager{Cells: link, Lines: bridge.PGLines{Pool: pool}, CallLog: &calllog.Store{Pool: pool, Log: log}, Names: contactStore,
		Log: log.With("component", "calls"), MediaIP: cfg.CellMediaIP()}
	calls.MediaPorts[0], calls.MediaPorts[1] = cfg.Cells.MediaRange()
	link.Media = calls
	calls.MediaFromLink = cfg.CellMediaAuto()
	if !calls.MediaIP.IsValid() && !calls.MediaFromLink {
		log.Warn("no address for the cells' speech: set OPENDECTCELL_CELLS_MEDIA_IP (the controller's address as the cells reach it); calls have no audio until then")
	}
	if dectStack != nil {
		windows = regwin.New(log.With("component", "registration"), nil, nil)
		// the stacks in worker processes, if asked (ADR 0003): a crash in the C code ends a worker
		newStack := dectStack
		var sup *worker.Supervisor
		sctx, sstop := context.WithCancel(ctx)
		defer func() { // after the core has closed its stacks
			sstop()
			if sup != nil {
				sup.Wait()
			}
		}()
		dect := &dectcore.Core{Roaming: roamingFrom(instance), Link: link, Records: &dectcore.Store{Pool: pool, Keyring: kr, NoPool: !cfg.Cells.HandsetPool},
			Reg: windows, Identity: cells, Log: log.With("component", "dect")}
		dectCore = dect
		orgs.Enforcer = dect
		if n := int(cfg.Cells.StackWorkers); n > 0 {
			sup = &worker.Supervisor{N: n, Log: log.With("component", "dect-workers")}
			sup.Start(sctx)
			newStack = func(env stack.Env, c stack.Config) stack.Stack { return sup.NewStack(env, c, dect.Clock) }
			log.Info("the DECT stacks run in worker processes", "workers", n)
		}
		dect.New = newStack
		calls.DECT = bridge.RouterOf(dect)
		dect.Originate = calls.HandsetCall
		calls.MWI = dect
		calls.Identity = dect
		calls.CallLists = dect
		dect.MWIFor = calls.MWIFor
		dect.Contacts = phonebook{contactStore}
		books.OnChange = func(owner uuid.UUID) { dect.ContactsChanged(owner) }
		contactStore.OnChange = func(owner *uuid.UUID) {
			if owner == nil {
				dect.ContactsChanged(uuid.Nil) // a shared contact: everybody's list
			} else {
				dect.ContactsChanged(*owner)
			}
		}
		windows.Done = func(w regwin.Window) {
			mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			// what the assistant asked for: the handset's name, and its device profile
			// with the model's name the pages show
			if _, err := pool.Exec(mctx, `UPDATE handset SET display_name = $2, model = nullif($3, ''), profile = nullif($4, '')
				WHERE id = $1`, w.Handset, w.Name, profiles.Default.Label(w.Profile), w.Profile); err != nil {
				log.Warn("the handset's name and profile could not be stored", "handset", w.Handset, "err", err)
				return
			}
			dect.HandsetChanged(w.Handset)
		}
		dctx, stop := context.WithCancel(ctx)
		dect.Start(dctx)
		// A safety net next to the calls the pages make: a deleted account or cell also changes whom
		// a handset's owner trusts.
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					dect.EnforceTrust()
				case <-dctx.Done():
					return
				}
			}
		}()
		defer dect.Wait() // after stop: the stacks close, their records are written
		defer stop()
		link.Mails = dect
	} else {
		log.Warn("built without libopendectcell (-tags libopendectcell): the DECT core is off, cell mails are dropped")
	}
	cln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Cells.Listen)
	if err != nil {
		return fmt.Errorf("cell control port: %w", err)
	}
	cellErr := make(chan error, 1)
	go func() { cellErr <- link.Serve(ctx, cln) }()

	sipMgr, sipAccts, err := startSIP(ctx, cfg, pool, kr, log, calls)
	if err != nil {
		return err
	}
	calls.SIP = bridge.DialerOf(sipMgr)
	defer sipMgr.Close()

	store := authStore(cfg, pool)
	store.EveryoneMayEnroll = everyoneMayEnroll
	store.Keyring = kr
	books.Keyring = kr
	handsets := &handset.Store{Pool: pool, Interval: func(ctx context.Context) time.Duration {
		ro, err := instance.Roaming(ctx)
		if err != nil {
			return 0
		}
		return time.Duration(ro.LocateValiditySec) * time.Second
	}}
	srv := &web.Server{
		Pool: pool, Auth: store, LocalLogin: cfg.Auth.Local.Enabled,
		BaseURL: base, SecureCookies: cfg.SecureCookies(), TrustedProxies: cfg.TrustedProxyPrefixes(),
		Log: log, Version: buildVersion(), OIDCName: cfg.Auth.OIDC.ProviderName,
		Cells: cells, CellLink: link, Orgs: orgs,
		SIPAccounts: sipAccts, SIP: sipMgr, Contacts: contactStore, Books: books,
		Registrations: windows,
		HandsetChanged: func(id uuid.UUID) {
			if dectCore != nil {
				dectCore.HandsetChanged(id)
			}
		},
		HandsetDeregister: func(id uuid.UUID, force bool) {
			if dectCore != nil {
				dectCore.Deregister(id, force)
			}
		},
		Settings: instance, Stats: &stats.Store{Pool: pool}, Handsets: handsets,
		Transfer: &transfer.Store{Pool: pool, Records: &dectcore.Store{Pool: pool, Keyring: kr}, Lines: sipAccts, Contacts: contactStore, Handsets: handsets},
		LogCtl:   logCtl, StartedAt: time.Now(),
		SMTPHost: cfg.SMTP.Host, MetricsToken: cfg.HTTP.MetricsToken,
	}
	if cfg.SMTP.Enabled() {
		srv.Mailer = &mailer.Mailer{Cfg: cfg.SMTP, Name: func() string {
			if in, err := instance.Instance(context.Background()); err == nil {
				return in.Name
			}
			return settings.DefaultName
		}}
	}
	go srv.RunLogExpiry(ctx)
	go srv.RunCellAlerts(ctx)
	go srv.RunMissedCallMails(ctx)
	go books.Run(ctx, log.Warn)
	go retention.Run(ctx, pool, instance, log)
	if cfg.Auth.OIDC.Enabled() {
		srv.OIDC = &auth.OIDC{Store: store, Config: cfg.Auth.OIDC,
			RedirectURL: strings.TrimRight(cfg.HTTP.BaseURL, "/") + "/auth/oidc/callback"}
	}
	h, err := srv.Handler()
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr: cfg.HTTP.Listen, Handler: h,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			if err := store.Cleanup(ctx); err != nil && ctx.Err() == nil {
				log.Warn("session cleanup", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("controller started", "version", buildVersion(), "listen", cfg.HTTP.Listen, "base_url", cfg.HTTP.BaseURL,
		"local_login", cfg.Auth.Local.Enabled, "oidc", cfg.Auth.OIDC.Enabled(),
		"cells_listen", cfg.Cells.Listen, "cells_public", cfg.CellPublicAddress(), "cell_ca_pin", cells.CA.Pin())
	if err := audit.RecordAs(ctx, pool, audit.System, audit.Entry{Action: "controller.start", Details: map[string]any{"version": buildVersion()}}); err != nil {
		log.Warn("audit", "err", err)
	}

	select {
	case err := <-errc:
		return err
	case err := <-cellErr:
		return fmt.Errorf("cell control port: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

// startSIP opens the SIP transports and starts every account's user agent.
// The endpoint takes the incoming calls (internal/bridge).
func startSIP(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, kr *secret.Keyring, log *slog.Logger, endpoint sip.Endpoint) (*sip.Manager, *sipaccount.Store, error) {
	lc := net.ListenConfig{}
	udp, err := lc.ListenPacket(ctx, "udp", cfg.SIP.UDPListen)
	if err != nil {
		return nil, nil, fmt.Errorf("SIP UDP %s: %w", cfg.SIP.UDPListen, err)
	}
	var tcp net.Listener
	if cfg.SIP.TCPListen != "" {
		if tcp, err = lc.Listen(ctx, "tcp", cfg.SIP.TCPListen); err != nil {
			udp.Close()
			return nil, nil, fmt.Errorf("SIP TCP %s: %w", cfg.SIP.TCPListen, err)
		}
	}
	lo, hi := cfg.SIP.RTPRange()
	sc := sip.Config{UDP: udp, TCP: tcp, RTP: &rtp.Ports{IP: netip.IPv4Unspecified(), Min: lo, Max: hi},
		Log: log.With("component", "sip"), UserAgent: "opendectcell-controller/" + buildVersion()}
	if cfg.SIP.MediaIP != "" {
		sc.MediaIP = netip.MustParseAddr(cfg.SIP.MediaIP) // validated
	}
	if w, ok := endpoint.(interface {
		AccountMWI(uuid.UUID, sip.MWI)
	}); ok {
		sc.OnMWI = w.AccountMWI // an account's mailbox: the handsets that have it as a line are told
	}
	history := &sipevents.Recorder{Pool: pool, Log: log.With("component", "sip-history")}
	sc.OnRegChange = history.Note // a line's registration history, for its page
	m, err := sip.New(sc, endpoint)
	if err != nil {
		return nil, nil, err
	}
	go history.Run(ctx)
	m.Start(ctx)
	accts := &sipaccount.Store{Pool: pool, Keyring: kr}
	reconcile := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		list, err := accts.RuntimeAccounts(rctx)
		if err != nil {
			log.Error("loading SIP accounts", "err", err)
		}
		m.SetAccounts(list)
	}
	accts.OnChange = reconcile
	reconcile()
	log.Info("SIP started", "udp", cfg.SIP.UDPListen, "tcp", cfg.SIP.TCPListen, "rtp", cfg.SIP.RTPPorts)
	return m, accts, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	m, err := db.NewMigrator(pool)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("migrate: want up, down [n] or status")
	}
	switch args[0] {
	case "up":
		did, err := m.Up(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "applied %d migration(s) %v\n", len(did), did)
	case "down":
		n := 1
		if len(args) > 1 {
			if n, err = strconv.Atoi(args[1]); err != nil || n < 1 {
				return errors.New("migrate down: n must be a positive number")
			}
		}
		did, err := m.Down(ctx, n)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "rolled back %d migration(s) %v\n", len(did), did)
	case "status":
		cur, latest, err := m.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "schema version %d of %d\n", cur, latest)
	default:
		return fmt.Errorf("migrate: unknown subcommand %q", args[0])
	}
	return nil
}

func keys(ctx context.Context, cmd string, cfg config.Config, pool *pgxpool.Pool, out io.Writer) error {
	if err := ensureSchema(ctx, pool, false, slog.Default()); err != nil {
		return err
	}
	mk, old, err := masterKeys(cfg)
	if err != nil {
		return err
	}
	actor := audit.Actor{Name: "cli"}
	switch cmd {
	case "rekey":
		if old == nil {
			return errors.New("rekey: set the new key as OPENDECTCELL_MASTER_KEY and the current one as OPENDECTCELL_MASTER_KEY_OLD")
		}
		n, err := db.Rekey(ctx, pool, mk, *old)
		if err != nil {
			return err
		}
		if err := audit.RecordAs(ctx, pool, actor, audit.Entry{Action: "secrets.rekey", Details: map[string]any{"data_key_count": n, "kek_id": mk.ID()}}); err != nil {
			return fmt.Errorf("rekey done, but the audit entry failed: %w", err)
		}
		fmt.Fprintf(out, "re-wrapped %d data key(s) under master key %s; remove OPENDECTCELL_MASTER_KEY_OLD now\n", n, mk.ID())
	case "rotate-dek":
		// a running controller holds the old keys in memory: after the rotation it could not open a rotated value, and
		// what it seals meanwhile would sit under a key the rotation deletes
		if err := db.CheckNoController(ctx, pool); err != nil {
			return fmt.Errorf("rotate-dek: %w (docker compose stop controller, run this, start it again)", err)
		}
		kr, err := db.LoadKeyring(ctx, pool, mk, old)
		if err != nil {
			return err
		}
		n, err := db.RotateDEK(ctx, pool, mk, kr)
		if err != nil {
			return err
		}
		id, _ := kr.ActiveID()
		if err := audit.RecordAs(ctx, pool, actor, audit.Entry{Action: "secrets.rotate_dek", Details: map[string]any{"value_count": n, "dek_id": id}}); err != nil {
			return fmt.Errorf("rotation done, but the audit entry failed: %w", err)
		}
		fmt.Fprintf(out, "re-encrypted %d secret(s) under data key %d\n", n, id)
	}
	return nil
}

func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	fmt.Fprint(stderr, "password (read from standard input): ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", errors.New("no password on standard input")
	}
	fmt.Fprintln(stderr)
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("user: want create, set-password or reset-2fa")
	}
	if err := ensureSchema(ctx, pool, false, slog.Default()); err != nil {
		return err
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "username")
	role := fs.String("role", "user", "admin or user")
	display := fs.String("display-name", "", "display name")
	email := fs.String("email", "", "e-mail address")
	mayAdd := fs.Bool("may-add-cells", false, "allow this user to enroll cells")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("user: --username is required")
	}
	store := authStore(cfg, pool)
	ctx = audit.WithActor(ctx, audit.Actor{Name: "cli"})
	if args[0] == "reset-2fa" { // for an admin who lost the app and the recovery codes
		u, err := store.UserByUsername(ctx, pool, *username)
		if err != nil {
			return fmt.Errorf("user %q: %w", *username, err)
		}
		if err := store.DisableTOTP(ctx, u.ID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "two-factor login of %s is off; they set it up again at their next login\n", u.Username)
		return nil
	}
	pw, err := readPassword(stdin, stderr)
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		u, err := store.CreateLocalUser(ctx, auth.NewLocalUser{Username: *username, DisplayName: *display, Email: *email,
			Password: pw, Role: auth.Role(*role), MayAddCells: *mayAdd})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created %s user %s (%s)\n", u.Role, u.Username, u.ID)
	case "set-password":
		u, err := store.UserByUsername(ctx, pool, *username)
		if err != nil {
			return fmt.Errorf("user %q: %w", *username, err)
		}
		if err := store.SetPassword(ctx, u.ID, pw, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "password set for %s; their sessions have been ended\n", u.Username)
	default:
		return fmt.Errorf("user: unknown subcommand %q", args[0])
	}
	return nil
}

func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	def := "http://127.0.0.1:8080/healthz"
	if l := os.Getenv("OPENDECTCELL_HTTP_LISTEN"); l != "" {
		if _, port, err := net.SplitHostPort(l); err == nil {
			def = "http://127.0.0.1:" + port + "/healthz"
		}
	}
	u := fs.String("url", def, "health URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// G704: the URL is the operator's own --url flag in a local health probe.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *u, nil) //nolint:gosec // see above
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // see above
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s", resp.Status)
	}
	return nil
}

// initSecrets creates the files a compose bring-up needs, without ever overwriting one.
//
// With --admin-dir the credentials are split: in --dir the controller's own files (the master key and the URL of the
// limited runtime role) and in --admin-dir the ones only the database and the migration step may see (the password
// of the role PostgreSQL was initialised with, and its URL). Without it everything goes into --dir and the controller
// connects as the initialising role, as it did before. A database_url that already exists is left alone, also when
// it names the initialising role: an existing installation keeps working until its owner removes that file
// (docs/deployment.md, "Database roles"). Files are 0600, except db_password (0644), which the database container
// reads as a different user.
func initSecrets(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init-secrets", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to write the controller's secrets to")
	adminDir := fs.String("admin-dir", "", "directory for the database's administrative credentials (default: --dir)")
	host := fs.String("db-host", "db", "database host for the URL")
	name := fs.String("db-name", "opendectcell", "database name")
	user := fs.String("db-user", "opendectcell", "the role PostgreSQL is initialised with (the schema's owner)")
	runtimeUser := fs.String("db-runtime-user", "opendectcell_app", "the limited role the controller runs as (with --admin-dir)")
	limited := fs.Bool("limited-role", false, "an existing database_url that names the owning role is replaced by one for the limited role (with --admin-dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("init-secrets: --dir is required")
	}
	split := *adminDir != "" && *adminDir != *dir
	if !split {
		*adminDir = *dir
	}
	for _, d := range []string{*dir, *adminDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	write := func(dir, file string, mode os.FileMode, gen func() (string, error)) (string, error) {
		p := filepath.Join(dir, file)
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // G304: path built from the operator's --dir
			return strings.TrimRight(string(b), "\r\n"), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		v, err := gen()
		if err != nil {
			return "", err
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // G304: as above
		if err == nil {
			err = f.Chmod(mode) // not reduced by the umask
		}
		if err != nil {
			return "", err
		}
		if _, err := f.WriteString(v + "\n"); err != nil {
			f.Close()
			return "", err
		}
		fmt.Fprintf(out, "created %s\n", p)
		return v, f.Close()
	}
	urlFor := func(u, pw string) func() (string, error) {
		return func() (string, error) {
			x := url.URL{Scheme: "postgres", User: url.UserPassword(u, pw), Host: *host, Path: "/" + *name, RawQuery: "sslmode=disable"}
			return x.String(), nil
		}
	}
	if _, err := write(*dir, "master_key", 0o600, secret.GenerateMasterKey); err != nil {
		return err
	}
	// the initialising role's password: an installation from before the split has it in --dir already
	pwGen := auth.NewToken
	if split {
		if b, err := os.ReadFile(filepath.Join(*dir, "db_password")); err == nil { //nolint:gosec // G304: as above
			old := strings.TrimRight(string(b), "\r\n")
			pwGen = func() (string, error) { return old, nil }
		}
	}
	pw, err := write(*adminDir, "db_password", 0o644, pwGen)
	if err != nil {
		return err
	}
	if !split {
		_, err = write(*dir, "database_url", 0o600, urlFor(*user, pw))
		return err
	}
	if _, err := write(*adminDir, "database_admin_url", 0o600, urlFor(*user, pw)); err != nil {
		return err
	}
	runtimePW, err := auth.NewToken()
	if err != nil {
		return err
	}
	if *limited {
		p := filepath.Join(*dir, "database_url")
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // G304: path built from the operator's --dir
			if u, _, err := db.RuntimeCredentials(strings.TrimSpace(string(b))); err == nil && u == *user {
				if err := os.Remove(p); err != nil {
					return err
				}
				fmt.Fprintf(out, "replacing %s (it named the role %s) with one for %s\n", p, u, *runtimeUser)
			}
		}
	}
	have, err := write(*dir, "database_url", 0o600, urlFor(*runtimeUser, runtimePW))
	if err != nil {
		return err
	}
	if u, _, err := db.RuntimeCredentials(have); err == nil && u == *user {
		fmt.Fprintf(out, "note: %s names the role %s, which owns the database: the controller keeps running with those rights; delete the file and run init again to give it a limited role (docs/deployment.md, \"Database roles\")\n", filepath.Join(*dir, "database_url"), u)
	}
	return nil
}

// roamingFrom gives the DECT core the admin's roaming settings.
func roamingFrom(st *settings.Store) func(context.Context) (dectcore.Roaming, error) {
	return func(ctx context.Context) (dectcore.Roaming, error) {
		r, err := st.Roaming(ctx)
		if err != nil {
			return dectcore.Roaming{}, err
		}
		sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
		return dectcore.Roaming{LocateValidity: sec(r.LocateValiditySec), ExpireAfter: sec(r.ExpireAfterSec), MovedAfter: sec(r.MovedAfterSec)}, nil
	}
}
