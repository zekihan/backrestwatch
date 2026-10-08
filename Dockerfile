FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /backrestwatch ./cmd/backrestwatch

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /backrestwatch /backrestwatch
USER 25100:25100
EXPOSE 8080
ENTRYPOINT ["/backrestwatch"]
LABEL org.opencontainers.image.source="https://github.com/zekihan/backrestwatch" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.title="backrestwatch" \
      org.opencontainers.image.description="Durable read-only PostgreSQL backup completion metadata freshness"
