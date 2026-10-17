# syntax=docker/dockerfile:1
# Static Go binary on distroless: no shell, no package manager, non-root.
# The OPA policy and prompts are embedded at build time.
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine3.24 AS build
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
