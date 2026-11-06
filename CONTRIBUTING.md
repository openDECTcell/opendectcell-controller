# Contributing to opendectcell-controller

Thank you for helping. Reports from real deployments (which cells, handsets and SIP providers work) and from the cell side
(box, handset, radio) are the most useful: for problems with a handset or a box use the
[opendectcell repository](https://github.com/openDECTcell/opendectcell/issues/new/choose).

## Reporting a bug

Open an issue with the controller version (page footer), how you run it, what you did, what you expected and what happened,
and the log (`docker compose logs controller`). **Remove personal data first:** phone numbers, SIP user names, passwords,
tokens, handset identities. A security problem is not a public issue: see [SECURITY.md](SECURITY.md).

## Building and testing

You need Go 1.27 or newer, a C11 compiler (for `internal/dect/cdect`, which links the C library from the `opendectcell`
submodule) and PostgreSQL 15 or newer for the database tests.

```sh
git clone --recurse-submodules https://github.com/openDECTcell/opendectcell-controller
cd opendectcell-controller
export OPENDECTCELL_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable'   # any scratch server
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go test -race -tags libopendectcell ./cmd/... ./internal/dect/...
golangci-lint run ./...      # the version pinned in .github/workflows/ci.yml
```

Without `OPENDECTCELL_TEST_DATABASE_URL` the database tests are skipped locally (and fail in CI). The CI workflow runs the same
gate plus the SIP interoperability tests and a Docker Compose bring-up.

## Rules for changes

- Every source file starts with `SPDX-License-Identifier: AGPL-3.0-or-later` (CI checks it). The C library in the submodule
  is GPL-2.0-or-later and stays in its own repository ([docs/licensing.md](docs/licensing.md)).
- A new Go module must be AGPL-compatible; say which licence in the pull request.
- No vendor firmware, disassembly or device trees, and no real handset identities, keys, phone numbers or addresses in code,
  tests or documents. Use the made-up values the tests already use.
- Keep changes small and tested; a change of the cell protocol or of the library interface updates the document that
  describes it ([docs/cell-protocol.md](docs/cell-protocol.md)).
- The project is developed with AI coding agents under the rules in [AGENTS.md](AGENTS.md); contributions by people are
  welcome on the same terms. By contributing you license your change under the project's licence.

## Conduct

See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
