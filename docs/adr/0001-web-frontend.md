# ADR 0001: Web frontend technology

Status: decided 2026-10-22 (task A6). Pending maintainer review.

## Decision

**Server-rendered HTML with Go `html/template`, plus htmx for partial updates.**
No SPA, no Node toolchain in the build. The REST/JSON API (`docs/api/openapi.yaml`)
exists alongside the HTML routes, shares their handlers' service layer and
authorisation checks, and is the interface for scripts and a future client.

## Options

| | Go templates + htmx | SPA (React/Vue/Svelte) on the JSON API |
|---|---|---|
| Build | `go build`; one vendored JS file (htmx) and one CSS file, embedded with `embed` | Node toolchain and an npm dependency tree in the build and in CI |
| Deliverable | One static binary serves everything | Binary plus a bundle, or a bundle embedded after a separate build step |
| Licence review (`AGENTS.md` §3) | One file to check | Hundreds of transitive npm packages to check, repeatedly |
| Security | No client-side auth state; the session is an HttpOnly cookie; output escaping by `html/template` contextual autoescape | Tokens or cookies handled in JS; XSS surface larger; CSRF still needed for cookie auth |
| Authorisation | Same server-side checks as the API (`AGENTS.md` §5); nothing to duplicate in the client | Server-side checks plus a client mirror of them for the UI |
| Live views (cell status, registration window countdown) | htmx polling or server-sent events into a fragment | Native |
| Fit for the UI's size | ~20 screens of lists and forms (`docs/ui/`) | Over-sized for it |
| AI-assisted iteration (`architecture.md` §7 reason for Go) | One language, one test runner | Two languages and two test stacks |

## Reasons

1. The UI is lists and forms: handsets, lines, accounts, cells, logs. The two
   live elements (registration countdown, cell status) are well served by htmx
   polling or SSE.
2. One binary and no Node toolchain keeps the Docker image small and the
   licence review tractable.
3. Authorisation stays in one place, on the server, which `AGENTS.md` §5
   requires anyway.

## Consequences

- Templates live in `internal/web/templates`, static assets in
  `internal/web/static`, both embedded.
- htmx is vendored as a single file with its licence header and version noted
  in `internal/web/static/README.md`. No CDN at runtime: installations may be
  offline, and a CDN would leak usage.
- CSRF protection is needed for cookie sessions: Go's
  `http.CrossOriginProtection` (Fetch-metadata based) plus `SameSite=Lax`
  cookies (`docs/threat-model.md`).
- If a richer client is ever needed, it can be built on the JSON API without
  changing the server.
