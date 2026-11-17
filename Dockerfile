# syntax=docker/dockerfile:1
# Multi-stage build: compile a static Go binary, then run it on a slim base
# with tini as PID 1 so signals reach the server.
FROM golang:1.26-bookworm AS build
WORKDIR /src

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-trimpath \
    GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags="-s -w" -o /out/server ./cmd/server && \
    go build -ldflags="-s -w" -o /out/demo ./cmd/demo

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata curl netcat-openbsd && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/demo /app/demo
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh && useradd -r -u 10001 appuser
USER appuser

ENV ADDR=:8080 \
    DATABASE_URL=postgres://ccdev:ccdev@db:5432/ccdev?sslmode=disable

EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["/app/server"]
