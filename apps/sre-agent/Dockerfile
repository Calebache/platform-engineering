# syntax=docker/dockerfile:1
# Static Go binary on distroless: no shell, no package manager, runs as
# non-root (uid 65532). Built for the node architecture (linux/amd64) even
# on Apple Silicon via --platform.
# Go 1.26.9 (2026-10-08) fixes stdlib CVE-2026-78667, -78669 and -97031; a
# security point release, so newer than the usual two-week rule. Pinned by digest.
FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine3.24@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
# The dashboard's HTML templates are embedded into the binary (go:embed).
COPY dashboard/ ./dashboard/
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sre-agent .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/sre-agent /sre-agent
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/sre-agent"]
CMD ["orchestrator"]
