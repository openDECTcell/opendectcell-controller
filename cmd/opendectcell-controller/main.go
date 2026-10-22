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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/config"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
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
  version
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
		return nil
	case "keygen":
		k, err := secret.GenerateMasterKey()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, k)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}

	cfg, err := config.Load(*cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, pool, log, rest)
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

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var lvl slog.Level
	lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
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

func serve(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	noMigrate := fs.Bool("no-migrate", false, "do not apply pending migrations; refuse to start if any are pending")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := ensureSchema(ctx, pool, !*noMigrate, log); err != nil {
		return err
	}
	mk, old, err := masterKeys(cfg)
	if err != nil {
		return err
	}
	if _, err := db.LoadKeyring(ctx, pool, mk, old); err != nil {
		return err
	}
	if old != nil {
		log.Warn("an old master key is configured; run `opendectcell-controller rekey`, then remove it")
	}

	store := authStore(cfg, pool)
	base, _ := url.Parse(cfg.HTTP.BaseURL)
	srv := &web.Server{
		Pool: pool, Auth: store, LocalLogin: cfg.Auth.Local.Enabled,
		BaseURL: base, SecureCookies: cfg.SecureCookies(), TrustedProxies: cfg.TrustedProxyPrefixes(),
		Log: log, Version: buildVersion(), OIDCName: cfg.Auth.OIDC.ProviderName,
	}
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
		"local_login", cfg.Auth.Local.Enabled, "oidc", cfg.Auth.OIDC.Enabled())
	audit.RecordAs(ctx, pool, audit.System, audit.Entry{Action: "controller.start", Details: map[string]any{"version": buildVersion()}})

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
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
		audit.RecordAs(ctx, pool, actor, audit.Entry{Action: "secrets.rekey", Details: map[string]any{"data_key_count": n, "kek_id": mk.ID()}})
		fmt.Fprintf(out, "re-wrapped %d data key(s) under master key %s; remove OPENDECTCELL_MASTER_KEY_OLD now\n", n, mk.ID())
	case "rotate-dek":
		kr, err := db.LoadKeyring(ctx, pool, mk, old)
		if err != nil {
			return err
		}
		n, err := db.RotateDEK(ctx, pool, mk, kr)
		if err != nil {
			return err
		}
		id, _ := kr.ActiveID()
		audit.RecordAs(ctx, pool, actor, audit.Entry{Action: "secrets.rotate_dek", Details: map[string]any{"value_count": n, "dek_id": id}})
		fmt.Fprintf(out, "re-encrypted %d secret(s) under data key %d\n", n, id)
	}
	return nil
}

func readPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	fmt.Fprint(stderr, "password (read from standard input): ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errors.New("no password on standard input")
	}
	fmt.Fprintln(stderr)
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("user: want create or set-password")
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
