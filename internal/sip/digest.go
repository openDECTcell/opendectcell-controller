// SPDX-License-Identifier: AGPL-3.0-or-later

package sip

import (
	"errors"
	"strings"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// applyDigest answers a 401 or 407 challenge on req (voip-requirements.md
// §3: MD5 and SHA-256, qop auth). When a server offers several challenges
// (RFC 8760), the strongest one we support wins.
func applyDigest(req *sip.Request, res *sip.Response, user, password string) error {
	inHdr, outHdr := "WWW-Authenticate", "Authorization"
	if res.StatusCode == sip.StatusProxyAuthRequired {
		inHdr, outHdr = "Proxy-Authenticate", "Proxy-Authorization"
	}
	var best *digest.Challenge
	rank := map[string]int{"": 1, "MD5": 1, "SHA-256": 2}
	for _, h := range res.GetHeaders(inHdr) {
		c, err := digest.ParseChallenge(h.Value())
		if err != nil {
			continue
		}
		c.Algorithm = strings.ToUpper(c.Algorithm)
		if rank[c.Algorithm] == 0 || (len(c.QOP) > 0 && !c.SupportsQOP("auth")) {
			continue
		}
		if best == nil || rank[c.Algorithm] > rank[best.Algorithm] {
			best = c
		}
	}
	if best == nil {
		return errors.New("sip: no supported digest challenge")
	}
	cred, err := digest.Digest(best, digest.Options{
		Method: req.Method.String(), URI: req.Recipient.Addr(), Username: user, Password: password,
	})
	if err != nil {
		return err
	}
	req.RemoveHeader(outHdr)
	req.AppendHeader(sip.NewHeader(outHdr, cred.String()))
	return nil
}
