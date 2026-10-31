// SPDX-License-Identifier: AGPL-3.0-or-later

package mailer

import (
	"strings"
	"testing"
	"time"
)

func TestMessage(t *testing.T) {
	m := string(Message("c@example.org", "u@example.org", "Größe\r\nBcc: x@evil", "line1\nline2", time.Unix(0, 0).UTC()))
	if strings.Contains(m, "\nBcc:") && !strings.Contains(m, "=?utf-8?q?") {
		t.Errorf("subject not encoded: %q", m)
	}
	if strings.Count(m, "\r\n\r\n") != 1 || !strings.HasSuffix(m, "line1\r\nline2\r\n") {
		t.Errorf("bad framing: %q", m)
	}
	if strings.Contains(strings.SplitN(m, "\r\n\r\n", 2)[0], "Bcc: x@evil") {
		t.Errorf("header injected: %q", m)
	}
}

func TestSendDisabled(t *testing.T) {
	var m *Mailer
	if m.Enabled() {
		t.Fatal("nil mailer enabled")
	}
	if err := (&Mailer{}).Send(nil, []string{"a@b.c"}, "s", "b"); err != ErrDisabled { //nolint:staticcheck // nil context: never used when disabled
		t.Fatalf("got %v", err)
	}
}
