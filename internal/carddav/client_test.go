// SPDX-License-Identifier: AGPL-3.0-or-later

package carddav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func multistatusOf(cards map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/book/</D:href><D:propstat><D:prop><D:getetag>"c"</D:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	for href, v := range cards {
		fmt.Fprintf(&b, `<D:response><D:href>%s</D:href><D:propstat><D:prop><D:getetag>"e-%s"</D:getetag><C:address-data><![CDATA[%s]]></C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, href, href, v)
	}
	b.WriteString(`</D:multistatus>`)
	return b.String()
}

func server(t *testing.T, h http.HandlerFunc) (*Client, string) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &Client{allowLoopback: true}, ts.URL + "/book/"
}

func TestFetch(t *testing.T) {
	var gotUser, gotPass, gotMethod, gotDepth string
	c, u := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotDepth = r.Method, r.Header.Get("Depth")
		gotUser, gotPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, multistatusOf(map[string]string{
			"/book/anna.vcf": "BEGIN:VCARD\nVERSION:3.0\nN:Schmidt;Anna\nTEL;TYPE=CELL:+49 170 111\nEND:VCARD\n",
			"/book/bad.vcf":  "BEGIN:VCARD\nVERSION:3.0\nN:Nobody;No\nEND:VCARD\n",
		}))
	})
	res, err := c.Fetch(context.Background(), u, "anna", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "REPORT" || gotDepth != "1" || gotUser != "anna" || gotPass != "pw" {
		t.Errorf("request: %s depth %s user %s %s", gotMethod, gotDepth, gotUser, gotPass)
	}
	if res.Total != 2 || len(res.Cards) != 1 || res.Cards[0].Href != "/book/anna.vcf" || res.Cards[0].ETag != "e-/book/anna.vcf" {
		t.Fatalf("cards: %+v", res)
	}
	if got := res.Cards[0].Contact; got.LastName != "Schmidt" || got.Numbers[0].Number != "+49170111" || got.Numbers[0].Kind != "mobile" {
		t.Errorf("contact: %+v", got)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "no number") {
		t.Errorf("skipped: %q", res.Skipped)
	}
}

func TestFetchSaysWhatWentWrong(t *testing.T) {
	for status, want := range map[int]string{401: "user name or password", 403: "user name or password", 404: "no address book", 500: "500", 200: "200"} {
		c, u := server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		if _, err := c.Fetch(context.Background(), u, "", ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: %v, want %q", status, err, want)
		}
	}
	c, u := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, "not xml at all")
	})
	if _, err := c.Fetch(context.Background(), u, "", ""); err == nil || !strings.Contains(err.Error(), "not what a CardDAV server sends") {
		t.Errorf("garbage: %v", err)
	}
}

func TestFetchUsesOnlyTheFirstFiveHundred(t *testing.T) {
	cards := map[string]string{}
	for i := 0; i < MaxCards+20; i++ {
		cards[fmt.Sprintf("/book/%d.vcf", i)] = fmt.Sprintf("BEGIN:VCARD\nN:L%d;F\nTEL:%d\nEND:VCARD\n", i, 100000+i)
	}
	c, u := server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, multistatusOf(cards))
	})
	res, err := c.Fetch(context.Background(), u, "", "")
	if err != nil || len(res.Cards) != MaxCards || res.Total != MaxCards+20 || !strings.Contains(strings.Join(res.Skipped, " "), "the first 500 are used") {
		t.Errorf("%d cards of %d, %q, %v", len(res.Cards), res.Total, res.Skipped, err)
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{"https://cloud.example.org/dav/": true, "http://cloud.example.org/": false, "ftp://x": false, "https://user:pw@x.org/": false, "x": false, "": false} {
		if err := CheckURL(u); (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
}

// The controller does not connect to its own machine or network because a user asked it to.
func TestTheClientDoesNotReachInside(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMultiStatus) }))
	defer ts.Close()
	// the httptest server is on 127.0.0.1: refused although it answers
	c := &Client{HTTP: nil}
	if _, err := c.Fetch(context.Background(), ts.URL+"/book/", "", ""); err == nil || !strings.Contains(err.Error(), "own network") {
		t.Errorf("loopback: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:80", "0.0.0.0:80", "[fe80::1]:80", "224.0.0.1:80", "[::ffff:127.0.0.1]:443"} {
		if err := c.control("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	for _, addr := range []string{"192.168.1.5:443", "10.0.0.1:443", "[fd00::1]:443", "172.16.0.1:443"} {
		if err := c.control("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed without the setting", addr)
		}
		if err := (&Client{AllowPrivate: true}).control("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused with the setting: %v", addr, err)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"} {
		if err := c.control("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
	// the setting does not open loopback or the metadata address
	if err := (&Client{AllowPrivate: true}).control("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("the setting opened the metadata address")
	}
}
