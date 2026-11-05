// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/alerts"
	"github.com/openDECTcell/opendectcell-controller/internal/stats"
)

func statusJSON(ok bool, finished time.Time, errText string) string {
	okS := "false"
	if ok {
		okS = "true"
	}
	return `{"version":1,"ok":` + okS + `,"started":"` + finished.Format(time.RFC3339) + `","finished":"` + finished.Format(time.RFC3339) + `","file":"opendectcell-20261105-031500.dump.gpg","size":9,"sha256":"ab",
"error":"` + errText + `","check":{"ran":true,"ok":true,"users":3,"tables":61,"error":""},"sftp":{"configured":true,"ok":true,"error":""},"keep":14,"next":"2026-11-06T03:15:00Z","at":"` + finished.Format(time.RFC3339) + `"}`
}

func TestBackupPage(t *testing.T) {
	dir := t.TempDir()
	e := newEnvWith(t, func(s *Server) { s.BackupDir = dir; s.Stats = &stats.Store{Pool: s.Pool} })
	a := e.adminBrowser(t)

	// no backup service: the page says so, and nothing is warned
	_, body := e.get(t, a, "/admin/backup")
	if !strings.Contains(body, "No backup service is running") {
		t.Fatalf("no state: %s", body)
	}
	if conds, err := e.app.backupConditions(ctx, time.Now()); err != nil || len(conds) != 0 {
		t.Fatalf("a missing service is not a failure: %v %+v", err, conds)
	}
	if e.app.backupAttention() != "" {
		t.Error("attention without a service")
	}

	// a good one
	now := time.Now().UTC().Truncate(time.Second)
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(statusJSON(true, now.Add(-2*time.Hour), "")), 0o600)
	os.WriteFile(filepath.Join(dir, "opendectcell-20261105-031500.dump.gpg"), []byte("encrypted"), 0o600)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("the master key"), 0o600)
	resp, body := e.get(t, a, "/admin/backup")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	for _, want := range []string{"The backups are in order", "restores", "3 users, 61 tables", "copied", "opendectcell-20261105-031500.dump.gpg", "14 newest are kept", "passphrase"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "secret.txt") {
		t.Error("a file that is not a backup is listed")
	}
	conds, err := e.app.backupConditions(ctx, now)
	if err != nil || len(conds) != 1 || conds[0].Status != alerts.Fine || conds[0].Kind != alerts.KindBackup {
		t.Errorf("a good backup: %v %+v", err, conds)
	}

	// the download: the file as it is, audited, administrators only, nothing else from the directory
	resp, body = e.get(t, a, "/admin/backup/opendectcell-20261105-031500.dump.gpg")
	if resp.StatusCode != http.StatusOK || body != "encrypted" || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	var n int
	e.store.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'backup.download' AND object_id = 'opendectcell-20261105-031500.dump.gpg'").Scan(&n)
	if n != 1 {
		t.Errorf("the download is audited %d times", n)
	}
	for _, p := range []string{"/admin/backup/secret.txt", "/admin/backup/status.json", "/admin/backup/..%2Fsecret.txt", "/admin/backup/opendectcell-20261105-999999.dump.gpg"} {
		if resp, _ := e.get(t, a, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	u := e.userBrowser(t)
	for _, p := range []string{"/admin/backup", "/admin/backup/opendectcell-20261105-031500.dump.gpg"} {
		if resp, body := e.get(t, u, p); resp.StatusCode == http.StatusOK || strings.Contains(body, "encrypted") {
			t.Errorf("a user got %s: %d", p, resp.StatusCode)
		}
	}
	if resp, body := e.get(t, e.browser(), "/admin/backup/opendectcell-20261105-031500.dump.gpg"); resp.StatusCode == http.StatusOK || body == "encrypted" {
		t.Error("a download without a login")
	}

	// a failed run, and an old one
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(statusJSON(false, now.Add(-time.Hour), "the dump failed: no connection")), 0o600)
	_, body = e.get(t, a, "/admin/backup")
	if !strings.Contains(body, "The last backup failed: the dump failed: no connection") || strings.Contains(body, "The backups are in order") {
		t.Errorf("a failed run on the page")
	}
	conds, _ = e.app.backupConditions(ctx, now)
	if len(conds) != 1 || conds[0].Status != alerts.Wrong || !conds[0].Who.Admins || !strings.Contains(conds[0].Title, "no connection") || conds[0].Href != "/admin/backup" {
		t.Errorf("a failed run: %+v", conds)
	}
	if !strings.Contains(e.app.backupAttention(), "failed") {
		t.Error("a failed run is not in the overview")
	}
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(statusJSON(true, now.Add(-50*time.Hour), "")), 0o600)
	if conds, _ = e.app.backupConditions(ctx, now); len(conds) != 1 || conds[0].Status != alerts.Wrong || !strings.Contains(conds[0].Title, "50 hours old") {
		t.Errorf("an old backup: %+v", conds)
	}
	// a state this version cannot read is an error of the source (the engine concludes nothing), not a failure of the backup
	os.WriteFile(filepath.Join(dir, "status.json"), []byte("{broken"), 0o600)
	if _, err := e.app.backupConditions(ctx, now); err == nil {
		t.Error("a broken state file was not reported")
	}
	if _, body = e.get(t, a, "/admin/backup"); !strings.Contains(body, "could not be read") {
		t.Error("a broken state file is not said on the page")
	}
}

func TestBackupOnTheOverviewForAdministrators(t *testing.T) {
	dir := t.TempDir()
	e := newEnvWith(t, func(s *Server) { s.BackupDir = dir; s.Stats = &stats.Store{Pool: s.Pool} })
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(statusJSON(false, time.Now(), "no space left")), 0o600)
	_, body := e.get(t, e.adminBrowser(t), "/")
	if !strings.Contains(body, "The last backup failed: no space left") || !strings.Contains(body, `href="/admin/backup"`) {
		t.Error("the overview does not say the backup failed")
	}
	if _, body = e.get(t, e.userBrowser(t), "/"); strings.Contains(body, "no space left") {
		t.Error("an ordinary user is told about the backups")
	}
}
