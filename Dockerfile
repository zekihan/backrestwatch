FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
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
