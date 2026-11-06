# Multi-stage: binario estático, imagen mínima.
FROM golang:1.26.10-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X ravenhub/internal/cli.Version=${VERSION}" \
      -o /out/ravenhub ./cmd/ravenhub \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X ravenhub/internal/cli.Version=${VERSION}" \
      -o /out/ravenctl ./cmd/ravenctl

# alpine 3.20 llegó a EOL en 2026-04-01: la capa aporta ca-certificates (raíz
# de confianza del TLS saliente) — sin parches no es base del trust store.
FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 ravenhub
COPY --from=build /out/ravenhub /out/ravenctl /usr/local/bin/
USER ravenhub
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=2 CMD wget -q -O /dev/null http://localhost:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/ravenhub"]
CMD ["serve"]
