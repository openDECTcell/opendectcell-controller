// SPDX-License-Identifier: AGPL-3.0-or-later

// Package carddav reads an address book from a CardDAV server (RFC 6352) into contacts, and keeps the copy up to
// date. It is a client only: the controller never serves CardDAV and never writes to the server.
package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

// MaxCards is how many cards of one book are used: a handset's phonebook holds about 500 entries.
const MaxCards = 500

const (
	maxResponse   = 16 << 20
	reportTimeout = 40 * time.Second
)

// Card is one entry of the book, as the server has it.
type Card struct {
	Href    string
	ETag    string
	Contact contacts.Contact
}

// Result is what a fetch found.
type Result struct {
	Cards   []Card
	Skipped []string // what could not be used, in words
	Total   int      // cards the server has; more than len(Cards) when the book is larger than MaxCards
}

// Client fetches books. The zero value refuses addresses on the machine's own network: a user must not make the
// controller probe its surroundings.
type Client struct {
	// AllowPrivate lets the controller reach private and unique-local addresses (a Nextcloud on the home network).
	// Loopback, link-local and unspecified addresses are never allowed: the cloud metadata service lives there.
	AllowPrivate bool
	// allowLoopback is for tests that run a server on 127.0.0.1.
	allowLoopback bool
	HTTP          *http.Client
}

var errBlocked = errors.New("that address is on this machine or its own network, which the controller does not connect to")

// control is the dialer's last look at an address, after the name has been resolved: it is what stops a name that
// resolves to a private address, and a redirect to one.
func (c *Client) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	switch {
	case c.allowLoopback && ip.IsLoopback():
		return nil
	case ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast():
		return errBlocked
	case ip.IsPrivate() && !c.AllowPrivate:
		return errBlocked
	}
	return nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Control: c.control}
	return &http.Client{
		Timeout:   reportTimeout,
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// CheckURL says whether u is an address of a book the controller will use: https, with a host, and nothing in front
// of the host.
func CheckURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return errors.New("that is not an address")
	}
	if p.Scheme != "https" {
		return errors.New("the address must start with https://")
	}
	if p.User != nil {
		return errors.New("put the user name and the password in their own fields, not in the address")
	}
	return nil
}

const queryBody = `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop><D:getetag/><C:address-data/></D:prop>
</C:addressbook-query>`

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				ETag string `xml:"getetag"`
				Data string `xml:"address-data"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// Fetch reads the book at bookURL.
func (c *Client) Fetch(ctx context.Context, bookURL, user, password string) (Result, error) {
	if err := CheckURL(bookURL); err != nil && (!c.allowLoopback || !strings.HasPrefix(bookURL, "http://")) {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "REPORT", bookURL, strings.NewReader(queryBody))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	req.Header.Set("User-Agent", "opendectcell-controller")
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("the server could not be reached: %w", unwrapURL(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{}, errors.New("the server refused the user name or password")
	case resp.StatusCode == http.StatusNotFound:
		return Result{}, errors.New("the server has no address book at that address")
	case resp.StatusCode != http.StatusMultiStatus:
		return Result{}, fmt.Errorf("the server answered %s, not a list of contacts", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return Result{}, err
	}
	if len(body) > maxResponse {
		return Result{}, errors.New("the answer is larger than 16 MB")
	}
	var ms multistatus
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&ms); err != nil {
		return Result{}, errors.New("the answer is not what a CardDAV server sends")
	}
	var res Result
	for _, r := range ms.Responses {
		var etag, data string
		for _, ps := range r.Propstat {
			if strings.Contains(ps.Status, " 200") {
				etag, data = strings.Trim(strings.TrimSpace(ps.Prop.ETag), `"`), ps.Prop.Data
			}
		}
		if strings.TrimSpace(data) == "" {
			continue // a collection, or a card the server will not give
		}
		res.Total++
		if len(res.Cards) >= MaxCards {
			continue
		}
		im := contacts.ParseVCards(strings.NewReader(data))
		res.Skipped = append(res.Skipped, im.Skipped...)
		if len(im.Contacts) == 0 {
			continue
		}
		res.Cards = append(res.Cards, Card{Href: r.Href, ETag: etag, Contact: im.Contacts[0]})
	}
	if res.Total > MaxCards {
		res.Skipped = append(res.Skipped, fmt.Sprintf("the book has %d contacts; the first %d are used", res.Total, MaxCards))
	}
	return res, nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
