#!/bin/sh
# Wait for PostgreSQL to accept connections before starting the app. The schema
# is applied idempotently by the app on boot, so no separate migration step is
# needed. Both the server and the demo binary talk to the database, so both
# wait for it.
set -eu

: "${DATABASE_URL:=postgres://ccdev:ccdev@db:5432/ccdev?sslmode=disable}"

case "${1:-}" in
  /app/server|/app/demo)
    # Extract host and port from the DSN: postgres://user:pass@HOST:PORT/db
    hostport=$(printf '%s' "$DATABASE_URL" | sed -n 's#^.*@\([^/]*\)/.*#\1#p')
    host=${hostport%%:*}
    port=${hostport##*:}
    [ -n "$host" ] || host=db
    [ -n "$port" ] || port=5432

    echo "waiting for postgres at ${host}:${port} ..."
    i=0
    until nc -z "$host" "$port" 2>/dev/null; do
      i=$((i + 1))
      if [ "$i" -ge 60 ]; then
        echo "postgres did not become ready in time" >&2
        exit 1
      fi
      sleep 1
    done
    echo "postgres is ready"
    ;;
esac

exec "$@"
