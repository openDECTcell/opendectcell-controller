// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func minimal() map[string]string {
	return map[string]string{
		"OPENDECTCELL_DATABASE_URL":  "postgres://u:p@db/opendectcell",
		"OPENDECTCELL_MASTER_KEY":    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"OPENDECTCELL_HTTP_BASE_URL": "https://dect.example.org",
	}
}

func TestMinimalEnv(t *testing.T) {
	c, err := Load("", env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Listen != ":8080" || !c.Auth.Local.Enabled || !c.SecureCookies() {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestMissingReportsEverything(t *testing.T) {
	_, err := Load("", env(nil))
	if err == nil {
		t.Fatal("empty config accepted")
	}
	for _, want := range []string{"OPENDECTCELL_DATABASE_URL", "master key", "OPENDECTCELL_HTTP_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestFileThenEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(`
http:
  listen: ":9000"
  base_url: "http://localhost:9000"
auth:
  session_idle: 1h
  oidc:
    issuer: https://idp.example.org/application/o/opendectcell/
    client_id: opendectcell
    admin_groups: [dect-admins]
`), 0o600)
	m := minimal()
	delete(m, "OPENDECTCELL_HTTP_BASE_URL")
	m["OPENDECTCELL_HTTP_LISTEN"] = ":9100"
	m["OPENDECTCELL_OIDC_USER_GROUPS"] = "staff, dect-users ,"
	c, err := Load(p, env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Listen != ":9100" {
		t.Errorf("env did not override file: %q", c.HTTP.Listen)
	}
	if c.Auth.SessionIdle != time.Hour || c.SecureCookies() {
		t.Errorf("file values lost: %+v", c.Auth)
	}
	if got := c.Auth.OIDC.UserGroups; len(got) != 2 || got[1] != "dect-users" {
		t.Errorf("list parsing: %q", got)
	}
	if !c.Auth.OIDC.Enabled() || c.Auth.OIDC.AdminGroups[0] != "dect-admins" {
		t.Errorf("oidc: %+v", c.Auth.OIDC)
	}
}

func TestUnknownFileKeyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("http:\n  lisen: \":1\"\n"), 0o600)
	if _, err := Load(p, env(minimal())); err == nil || !strings.Contains(err.Error(), "lisen") {
		t.Fatalf("typo accepted: %v", err)
	}
}

func TestSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "mk")
	os.WriteFile(kf, []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\n"), 0o600)
	m := minimal()
	delete(m, "OPENDECTCELL_MASTER_KEY")
	m["OPENDECTCELL_MASTER_KEY_FILE"] = kf
	c, err := Load("", env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Secrets.MasterKey, "BBBB") || strings.HasSuffix(c.Secrets.MasterKey, "\n") {
		t.Errorf("file secret: %q", c.Secrets.MasterKey)
	}
	m["OPENDECTCELL_MASTER_KEY"] = "x"
	if _, err := Load("", env(m)); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("both set: %v", err)
	}
}

func TestBadValuesDoNotEchoSecrets(t *testing.T) {
	m := minimal()
	m["OPENDECTCELL_DATABASE_MAX_CONNS"] = "lots"
	m["OPENDECTCELL_LOG_LEVEL"] = "loud"
	_, err := Load("", env(m))
	if err == nil || !strings.Contains(err.Error(), "lots") || !strings.Contains(err.Error(), "loud") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "u:p@db") {
		t.Errorf("secret echoed: %v", err)
	}
}

func TestOIDCValidation(t *testing.T) {
	m := minimal()
	m["OPENDECTCELL_OIDC_ISSUER"] = "http://idp.example.org"
	_, err := Load("", env(m))
	if err == nil || !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "client ID") {
		t.Fatalf("got %v", err)
	}
	m["OPENDECTCELL_OIDC_ISSUER"] = "http://127.0.0.1:5556"
	m["OPENDECTCELL_OIDC_CLIENT_ID"] = "c"
	if _, err := Load("", env(m)); err != nil {
		t.Errorf("loopback http issuer rejected: %v", err)
	}
}

func TestNoLoginMethod(t *testing.T) {
	m := minimal()
	m["OPENDECTCELL_AUTH_LOCAL_ENABLED"] = "false"
	if _, err := Load("", env(m)); err == nil || !strings.Contains(err.Error(), "no login method") {
		t.Fatalf("got %v", err)
	}
}

func TestEnvNamesIncludeFileVariants(t *testing.T) {
	names := strings.Join(EnvNames(), " ")
	for _, n := range []string{"OPENDECTCELL_MASTER_KEY_FILE", "OPENDECTCELL_OIDC_CLIENT_SECRET_FILE", "OPENDECTCELL_DATABASE_URL_FILE"} {
		if !strings.Contains(names, n) {
			t.Errorf("missing %s", n)
		}
	}
	if strings.Contains(names, "OPENDECTCELL_LOG_LEVEL_FILE") {
		t.Error("non-secret has a _FILE variant")
	}
}

func TestEveryVariableIsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range EnvNames() {
		if strings.HasSuffix(n, "_FILE") {
			continue // covered by the 🔒 rule
		}
		if !strings.Contains(string(b), "`"+n+"`") {
			t.Errorf("%s is not in docs/configuration.md", n)
		}
	}
}

func TestCellPublicAddress(t *testing.T) {
	c, err := Load("", env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CellPublicAddress(); got != "dect.example.org:7300" {
		t.Errorf("default: %s", got)
	}
	m := minimal()
	m["OPENDECTCELL_CELLS_PUBLIC_ADDRESS"] = "cells.example.org:443"
	c, _ = Load("", env(m))
	if got := c.CellPublicAddress(); got != "cells.example.org:443" {
		t.Errorf("override: %s", got)
	}
	m["OPENDECTCELL_CELLS_PUBLIC_ADDRESS"] = "no-port"
	if _, err := Load("", env(m)); err == nil {
		t.Error("address without port accepted")
	}
}
