# Licensing

This answers audit findings LIC-01 and LIC-02. It states facts of the repository; it is not legal advice, and a lawyer's view
is still wanted before a public release.

## The controller

opendectcell-controller is **AGPL-3.0-or-later**. Every source file carries `SPDX-License-Identifier: AGPL-3.0-or-later` (the CI
and `ci-local.sh` check it). Vendored web assets (Tabler, Leaflet, icons) keep their own licences next to them under
`internal/web/static/vendor/`.

**Source offer (AGPL section 13).** The footer of every page links to the source repository. A deployment that runs a modified
version must offer its users *that* version's source; the link points at the repository, not at one revision, so such a
deployment has to change the link (the footer text is in `internal/web/templates/_layout.html`) to where its source is. The
version string in the footer and in `controller started` in the log tells which build runs.

## libopendectcell inside the binary (LIC-02)

The DECT core is `libopendectcell` from the openDECTcell repository, linked statically through cgo into the controller binary (the image
is built with `LIBOPENDECTCELL=1`).

- Every file of `libopendectcell` is **GPL-2.0-or-later** (openDECTcell `docs/licensing.md`: 48 files, all with that SPDX identifier). GPL-2.0-or-later
  can be combined with AGPL-3.0: the "or later" lets the combination be distributed under GPL-3.0 terms, and AGPL-3.0 section 13
  covers the network use. GPL-2.0-**only** code could not be linked in; there is none in user space.
- The **kernel modules** of openDECTcell are GPL-2.0-only and are not linked into the controller: they run on the box, and the
  controller never contains them.
- The firmware of the DECT chip is the user's own, taken from a FRITZ!OS image; it is not in either repository and not in the image.

## Other dependencies

Dependencies must be AGPL-compatible (`AGENTS.md` §3); the Go modules are in `go.mod`, and a new one is checked for its licence
before it is added (the QR code module `rsc.io/qr` is BSD-3-Clause). `github.com/gotranspile/g722` is a Go translation of the
G.722 codec; its licence is part of its module. A complete, machine-made list of licences is not kept yet. The base image is distroless (Debian packages and their licences); PostgreSQL has its own licence and runs as
a separate container.
