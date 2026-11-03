# SPDX-License-Identifier: AGPL-3.0-or-later
# Multi-stage build: a static binary on a distroless, non-root base.
# By default the build is CGO_ENABLED=0 and the binary has no DECT core.
# --build-arg LIBOPENDECTCELL=1 (needs the openDECTcell submodule checked out) links
# libopendectcell with cgo, statically, and turns the DECT core on (C3).

FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS build
ARG VERSION=dev
ARG LIBOPENDECTCELL=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY third_party ./third_party
RUN if [ "$LIBOPENDECTCELL" = 1 ]; then \
      test -f third_party/opendectcell/src/libopendectcell/dect_base.c || { echo "LIBOPENDECTCELL=1 needs the opendectcell submodule"; exit 1; }; \
      CGO_ENABLED=1 go build -trimpath -tags libopendectcell,netgo,osusergo \
        -ldflags "-s -w -linkmode external -extldflags -static -X main.version=${VERSION}" \
        -o /out/opendectcell-controller ./cmd/opendectcell-controller; \
    else \
      CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/opendectcell-controller ./cmd/opendectcell-controller; \
    fi \
 && mkdir -p /out/secrets /out/dbsecrets /out/tmp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/opendectcell-controller /usr/local/bin/opendectcell-controller
# A named volume mounted here starts out owned by the non-root user, so
# `init-secrets` can write to it. /dbsecrets holds the database's administrative
# credentials; only the database, `init` and `migrate` mount it.
COPY --from=build --chown=65532:65532 /out/secrets /secrets
COPY --from=build --chown=65532:65532 /out/dbsecrets /dbsecrets
COPY --from=build --chown=65532:65532 /out/tmp /tmp
USER 65532:65532
EXPOSE 8080 7300
HEALTHCHECK --interval=10s --timeout=4s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/opendectcell-controller", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/opendectcell-controller"]
CMD ["serve"]
