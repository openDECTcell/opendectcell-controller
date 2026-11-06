# Third-party components

The controller is AGPL-3.0-or-later. It uses the components below; each keeps its own licence, which is the one to read.
This list is kept by hand ([docs/licensing.md](docs/licensing.md) explains how the licences fit together).

**In the repository**

| Component | Used for | Licence |
|---|---|---|
| [Tabler](https://tabler.io) 1.6 (`internal/web/static/vendor/tabler/`, with its icons) | the web interface's styles and scripts | MIT (licence file beside it) |
| [Leaflet](https://leafletjs.com) 1.9 (`internal/web/static/vendor/leaflet/`) | the optional public map | BSD-2-Clause (licence file beside it) |
| [openDECTcell](https://github.com/openDECTcell/opendectcell) (`third_party/opendectcell`, a git submodule) | `libopendectcell`, the DECT protocol library, linked into the controller through cgo | GPL-2.0-or-later |

**Go modules linked into the binary** (`go.mod` is authoritative)

| Licence | Modules |
|---|---|
| Apache-2.0 | `coreos/go-oidc`, `go-jose/go-jose`, `go.yaml.in/yaml/v3` and the OpenAPI helpers `oasdiff/yaml3`, `santhosh-tekuri/jsonschema`, `go-openapi/jsonpointer` |
| BSD-3-Clause | `emiago/sipgo`, `google/uuid`, `miekg/dns`, `rsc.io/qr`, `golang.org/x/*` |
| MIT | `getkin/kin-openapi`, `icholy/digest`, `jackc/pgx` (and its helpers), `pion/rtp`, `rtcp`, `sdp`, `srtp` and their helpers |
| see below | `gotranspile/g722` |

`github.com/gotranspile/g722` (`internal/media/codec/g722.go`) is a Go translation of the G.722 codec by Dmitry Goncharov
(2010), with parts by Steve Underwood (2005, public-domain dedication), based on a single-channel codec from Carnegie Mellon
University (Lu and Hauptmann, 1993). The module has no licence file; its README says "Distributed under the BSD license".
The media engine uses it as the G.722 codec when it has to convert audio; where both ends speak G.722 the audio is passed
through unchanged. The CMU notice is kept here. The project's own G.722 in `libopendectcell` could replace it
([docs/media-engine.md](docs/media-engine.md)).

**Container image:** the Go build image and the distroless runtime image (Debian packages with their own licences);
PostgreSQL runs as a separate container under its own licence.
