# Asterisk for the SIP interop test

CI (`.github/workflows/ci.yml`, job `interop`) installs Asterisk from the
Ubuntu package, copies these files over the package's, and runs
`go test -tags interop -run Interop ./internal/sip/`. Two users, 6001 and
6002, are registered by the controller and call each other through
Asterisk, which relays the media.

To run it by hand: install Asterisk, copy the `*.conf` files to
`/etc/asterisk/`, restart it, then
`OPENDECTCELL_INTEROP_SIP=127.0.0.1:5060 go test -tags interop -run Interop ./internal/sip/`.
