FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:dbaa054703263d06c8b49576667418473b6231f8c0fa69dee103207fdb8e3654 AS build
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
