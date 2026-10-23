# syntax=docker/dockerfile:1
# Static Go binary on distroless: no shell, no package manager, non-root.
# The OPA policy and prompts are embedded at build time.
# Go 1.26.10 (2026-10-20) fixes stdlib CVE-2026-111943 (crypto/x509), after
# 1.26.9 fixed CVE-2026-78667, -78669 and -97031. Security point releases, so
# newer than the usual two-week rule. Pinned by digest; bump together with
# the toolchain line in go.mod.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine3.24@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY policy/ policy/
COPY prompts/ prompts/
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mcp-server .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/mcp-server /mcp-server
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/mcp-server"]
CMD ["serve"]
