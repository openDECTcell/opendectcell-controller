// SPDX-License-Identifier: AGPL-3.0-or-later

package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/backup"
)

const good = `{"version":1,"ok":true,"started":"2026-11-05T03:15:00Z","finished":"2026-11-05T03:16:10Z","file":"opendectcell-20261105-031500.dump.gpg","size":1234,
"sha256":"ab","error":"","check":{"ran":true,"ok":true,"users":3,"tables":60,"error":""},"sftp":{"configured":true,"ok":true,"error":""},"keep":14,
"next":"2026-11-06T03:15:00Z","at":"2026-11-05T03:16:10Z"}`

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadAndProblem(t *testing.T) {
	dir := t.TempDir()
	if _, err := backup.Read(dir); !errors.Is(err, backup.ErrNone) {
		t.Fatalf("no state file: %v", err)
	}
	write(t, dir, "status.json", good)
	st, err := backup.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.File != "opendectcell-20261105-031500.dump.gpg" || st.Size != 1234 || !st.Check.OK || st.Check.Users != 3 || !st.SFTP.Configured || !st.SFTP.OK || st.Keep != 14 {
		t.Errorf("status: %+v", st)
	}
	at := time.Date(2026, 11, 5, 3, 16, 10, 0, time.UTC)
	if !st.LastGood.Equal(at) {
		t.Errorf("last good: %v", st.LastGood)
	}
	if p := st.Problem(at.Add(35 * time.Hour)); p != "" {
		t.Errorf("35 hours old: %q", p)
	}
	if p := st.Problem(at.Add(37 * time.Hour)); !strings.Contains(p, "37 hours old") {
		t.Errorf("37 hours old: %q", p)
	}
	// a failed last run is a problem whatever the age, with the reason
	failed := strings.Replace(strings.Replace(good, `"ok":true,"started"`, `"ok":false,"started"`, 1), `"error":"","check"`, `"error":"the dump failed: no connection","check"`, 1)
	write(t, dir, "status.json", failed)
	st, _ = backup.Read(dir)
	if p := st.Problem(at); !strings.Contains(p, "failed") || !strings.Contains(p, "no connection") {
		t.Errorf("failed run: %q", p)
	}
	// ... and the last good backup is then the newest file, not this run
	if !st.LastGood.IsZero() {
		t.Errorf("last good without any file: %v", st.LastGood)
	}
	write(t, dir, "opendectcell-20261104-031500.dump.gpg", "x")
	old := time.Date(2026, 11, 4, 3, 16, 0, 0, time.UTC)
	os.Chtimes(filepath.Join(dir, "opendectcell-20261104-031500.dump.gpg"), old, old)
	write(t, dir, "opendectcell-notours.dump.gpg", "x") // not a name the service makes
	os.Chtimes(filepath.Join(dir, "opendectcell-notours.dump.gpg"), at, at)
	if st, _ = backup.Read(dir); !st.LastGood.Equal(old) {
		t.Errorf("last good from the files: %v", st.LastGood)
	}
	// the service has just started and made none: nothing to say for a day and a half, then a problem
	fresh := `{"version":1,"ok":false,"started":"","finished":"","file":"","size":0,"sha256":"","error":"no backup has run yet","check":{},"sftp":{},"keep":14,"next":"2026-11-06T03:15:00Z","at":"2026-11-05T12:00:00Z"}`
	write(t, dir, "status.json", fresh)
	st, _ = backup.Read(dir)
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if p := st.Problem(start.Add(30 * time.Hour)); p != "" {
		t.Errorf("just started: %q", p)
	}
	if p := st.Problem(start.Add(40 * time.Hour)); !strings.Contains(p, "No backup has been made yet") {
		t.Errorf("never ran: %q", p)
	}
	// files that are not ours
	for name, content := range map[string]string{"a": "{not json", "b": `{"version":2}`, "c": `{"version":1,"error":"` + strings.Repeat("x", 70<<10) + `"}`} {
		write(t, dir, "status.json", content)
		if _, err := backup.Read(dir); err == nil || errors.Is(err, backup.ErrNone) {
			t.Errorf("%s: a state file that is not ours was read (%v)", name, err)
		}
	}
}

func TestOpenOnlyOurFiles(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, dir, "opendectcell-20261105-031500.dump.gpg", "encrypted")
	write(t, outside, "secret.txt", "the master key")
	write(t, dir, "status.json", "{}")
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "opendectcell-20261105-040000.dump.gpg"))
	os.Mkdir(filepath.Join(dir, "opendectcell-20261105-050000.dump.gpg"), 0o700)
	f, fi, err := backup.Open(dir, "opendectcell-20261105-031500.dump.gpg")
	if err != nil || fi.Size() != 9 {
		t.Fatalf("a backup: %v", err)
	}
	f.Close()
	for _, name := range []string{"../secret.txt", "status.json", "opendectcell-20261105-040000.dump.gpg", "opendectcell-20261105-050000.dump.gpg", "opendectcell-20261105-999999.dump.gpg",
		"opendectcell-1-2.dump.gpg", "", "/etc/passwd", "../" + filepath.Base(outside) + "/secret.txt", "opendectcell-20261105-031500.dump.gpg/../status.json", "opendectcell-20261105-031500.dump.gpg\x00"} {
		if f, _, err := backup.Open(dir, name); err == nil {
			f.Close()
			t.Errorf("opened %q", name)
		}
	}
	files := backup.Files(dir)
	if len(files) != 1 || files[0].Name() != "opendectcell-20261105-031500.dump.gpg" {
		t.Errorf("files: %v", files)
	}
	write(t, dir, "opendectcell-20261106-031500.dump.gpg", "newer")
	if files = backup.Files(dir); len(files) != 2 || files[0].Name() != "opendectcell-20261106-031500.dump.gpg" {
		t.Errorf("newest first: %v", files)
	}
}
