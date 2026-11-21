# syntax=docker/dockerfile:1

# --- Frontend ---
FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Backend ---
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X younithm/internal/version.Version=${VERSION}" -o /out/younithm ./cmd/younithm

# --- Runtime ---
FROM alpine:3.22
RUN apk add --no-cache su-exec tzdata ca-certificates
COPY --from=build /out/younithm /usr/local/bin/younithm
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Unraid defaults: nobody:users
ENV PUID=99 PGID=100 \
    YOUNITHM_CONFIG_DIR=/config \
    YOUNITHM_ADDR=:2015
VOLUME /config
EXPOSE 2015

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:2015/healthz >/dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["younithm"]
