# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.4.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X cronbeat.dev/cronbeat-agent/internal/version.Version=${VERSION}" \
      -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]
