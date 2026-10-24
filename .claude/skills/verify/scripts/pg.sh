#!/usr/bin/env bash
set -euo pipefail
name=${DOLMEN_VERIFY_PG_NAME:-dolmen-verify-pg}
port=${DOLMEN_VERIFY_PG_PORT:-55432}
case "${1:-start}" in
start)
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" -e POSTGRES_USER=dolmen -e POSTGRES_PASSWORD=dolmen-test -e POSTGRES_DB=dolmen_test -p "127.0.0.1:$port:5432" postgres:17 >/dev/null
  for _ in $(seq 60); do
    docker exec "$name" pg_isready -U dolmen -d dolmen_test >/dev/null 2>&1 && break
    sleep 1
  done
  docker exec "$name" psql -U dolmen -d dolmen_test -v ON_ERROR_STOP=1 -q -c "CREATE ROLE dolmen_query NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE dolmen_backend LOGIN PASSWORD 'backend-test'; GRANT dolmen_query TO dolmen_backend WITH INHERIT FALSE, SET TRUE; GRANT CONNECT, CREATE ON DATABASE dolmen_test TO dolmen_backend;"
  echo "export DOLMEN_TEST_PG_DSN='postgres://dolmen_backend:backend-test@127.0.0.1:$port/dolmen_test?sslmode=disable' DOLMEN_TEST_PG_REQUIRED=1 DOLMEN_TEST_PG_QUERY_ROLE=dolmen_query"
  ;;
stop)
  docker rm -f "$name" >/dev/null
  ;;
esac
