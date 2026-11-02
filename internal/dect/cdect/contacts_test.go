// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"strings"
	"testing"
)

// A record gets a Contact List of its own from a contacts file, the library refuses text that is
// not one, and what a handset changes in it comes out as a journal line the next Tick.
func TestRecordContactList(t *testing.T) {
	e := fetchEnv(t)
	h := e.host
	if h.contactCount(2) != -1 {
		t.Fatal("a list before one was given")
	}
	text := "#opendectcell-contacts 1 seq=0\n" +
		"0000002a\t-\tMeyer\tEva\t0\t*\t04:0171\n" +
		"0000002b\t7\tMüller\t\t0\t*\t08:030\t02:100\n"
	if !h.SetContacts(2, text) || h.contactCount(2) != 2 {
		t.Fatalf("SetContacts: count %d", h.contactCount(2))
	}
	if h.contactCount(3) != -1 {
		t.Error("another record has a list")
	}
	// not a contacts file: refused, the list stays
	if h.SetContacts(2, "hello\n") || h.contactCount(2) != 2 {
		t.Errorf("a bad file was taken: %d", h.contactCount(2))
	}
	// a new file replaces the list
	if !h.SetContacts(2, "#opendectcell-contacts 1 seq=0\n0000002a\t-\tMeyer\tEva\t0\t*\t04:0171\n") || h.contactCount(2) != 1 {
		t.Errorf("a new file: %d", h.contactCount(2))
	}
	// a handset adds a contact: a journal line comes out
	if !h.testContactPut(2, "Neu", "0301") {
		t.Fatal("the put failed")
	}
	h.Tick(e.now)
	if len(e.changes) != 1 || !strings.Contains(e.changes[0], "\tput\t") || !strings.Contains(e.changes[0], "Neu") ||
		!strings.Contains(e.changes[0], "08:0301") && !strings.Contains(e.changes[0], "04:0301") {
		t.Errorf("changes %q", e.changes)
	}
	h.Tick(e.now)
	if len(e.changes) != 1 {
		t.Errorf("a change was reported twice: %q", e.changes)
	}
	if h.SetContacts(40, "#opendectcell-contacts 1 seq=0\n") {
		t.Error("a record number beyond the table has a list")
	}
}

// A call lists file replaces the lists; a handset with a new unread missed call is counted (and told
// when it is on a record); text that is not a call lists file changes nothing.
func TestSetCalls(t *testing.T) {
	e := fetchEnv(t)
	h := e.host
	text := "#opendectcell-calls 1\n" +
		"1\t5\t1\t1\t261102101500\t0\t" + strings.Repeat("00", 3) + "01" + strings.Repeat("00", 29) + "\t0301234\tAnna\n" +
		"3\t6\t0\t1\t261102101000\t0\t" + strings.Repeat("00", 3) + "01" + strings.Repeat("00", 29) + "\t0301234\tAnna\n"
	if !h.SetCalls(text) {
		t.Fatal("SetCalls refused a call lists file")
	}
	if h.callsMissed(3, 1) != 1 || h.callsMissed(2, 1) != 0 {
		t.Errorf("unread missed: terminal 3 line 1 = %d, terminal 2 = %d", h.callsMissed(3, 1), h.callsMissed(2, 1))
	}
	if h.SetCalls("hello\n") || h.callsMissed(3, 1) != 1 {
		t.Error("a bad file changed the lists")
	}
	if !h.SetCalls("#opendectcell-calls 1\n") || h.callsMissed(3, 1) != 0 {
		t.Error("an empty file did not empty the lists")
	}
}
