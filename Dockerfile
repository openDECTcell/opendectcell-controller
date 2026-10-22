# SPDX-License-Identifier: AGPL-3.0-or-later
# Multi-stage build: a static binary on a distroless, non-root base.
# The controller does not link libopendectcell yet (C3), so the build is
# CGO_ENABLED=0; when it does, this becomes a static cgo build.

FROM golang:1.27.3-bookworm AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/opendectcell-controller ./cmd/opendectcell-controller \
 && mkdir -p /out/secrets /out/tmp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/opendectcell-controller /usr/local/bin/opendectcell-controller
# A named volume mounted here starts out owned by the non-root user, so
# `init-secrets` can write to it.
COPY --from=build --chown=65532:65532 /out/secrets /secrets
COPY --from=build --chown=65532:65532 /out/tmp /tmp
USER 65532:65532
EXPOSE 8080 7300
HEALTHCHECK --interval=10s --timeout=4s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/opendectcell-controller", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/opendectcell-controller"]
CMD ["serve"]
