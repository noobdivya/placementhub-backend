# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
# go.mod may ask for a newer toolchain than the image ships; GOTOOLCHAIN=auto fetches it.
FROM golang:1.25 AS build
ENV GOTOOLCHAIN=auto CGO_ENABLED=0
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed \
 && go build -trimpath -ldflags="-s -w" -o /out/vapid ./cmd/vapid

# ---- runtime ----------------------------------------------------------------
FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S app && adduser -S -G app -u 10001 app \
 && mkdir -p /data/uploads && chown -R app:app /data

COPY --from=build /out/api /out/seed /out/vapid /usr/local/bin/

USER app
ENV APP_ENV=production HTTP_ADDR=:8080 UPLOAD_DIR=/data/uploads
VOLUME ["/data/uploads"]
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

# Migrations run automatically at startup.
ENTRYPOINT ["api"]
