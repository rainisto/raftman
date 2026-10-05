# PostgreSQL backend (fork addition)

This fork adds an optional **PostgreSQL** storage backend alongside upstream's
embedded SQLite store. The JSON API, syslog frontends and web UI are identical
for both backends — only the internal storage changes.

Use PostgreSQL when the log volume grows large enough that SQLite query latency
becomes a problem (e.g. the web UI times out on millions of rows). For small or
moderate volumes, the default SQLite backend needs no extra container and is
simpler to operate.

## Enabling it

Select the backend with the `-backend` flag (or a positional-free URL). The
scheme decides the store:

```sh
# SQLite (default, upstream behaviour)
raftman -backend "sqlite:///var/lib/raftman/logs.db?retention=30d"

# PostgreSQL
raftman -backend "postgres://user:pass@host:5432/dbname?sslmode=disable&retention=30d"
```

`postgres://` and `postgresql://` are both accepted. The table and indexes are
created automatically on first start.

## Supported query parameters

These are parsed by raftman and stripped from the connection string before it is
handed to the driver; everything else (e.g. `sslmode`, `connect_timeout`) is
passed through to PostgreSQL unchanged.

| Parameter         | Default | Meaning                                             |
|-------------------|---------|-----------------------------------------------------|
| `retention`       | `INF`   | Purge entries older than this (`INF`, or `w`/`d`/`h`/`m`, e.g. `30d`). |
| `batchSize`       | `32`    | Extra queued entries folded into one insert transaction. |
| `insertQueueSize` | `512`   | Entries buffered between ingest and the writer.     |
| `queryQueueSize`  | `16`    | Concurrent queries allowed (also sizes the connection pool). |
| `timeout`         | `5s`    | Deadline for a single query.                        |

## Full text search difference

The only user-visible behavioural difference is the `Message` search syntax:

- **SQLite** uses SQLite FTS4 `MATCH` syntax.
- **PostgreSQL** uses `websearch_to_tsquery('simple', ...)` (e.g. `error OR fail`,
  quoted phrases, `-exclude`). This tolerates arbitrary user input without
  syntax errors.

Clients that send FTS4-specific query syntax to the `Message` field must be
adjusted when pointing at a PostgreSQL-backed instance.

## docker-compose example

```yaml
services:
  raftman:
    image: rainisto/raftman:1.1.0   # or your own build
    restart: unless-stopped
    # Exec/list form so the "&" in the URL is passed verbatim (not treated as a
    # shell background operator).
    entrypoint:
      - /usr/local/bin/raftman
      - -backend
      - "postgres://raftman:${RAFTMAN_DB_PASSWORD:-raftman}@raftman-db:5432/raftman?sslmode=disable&retention=30d"
    depends_on:
      - raftman-db
    ports:
      - "514:514/udp"
      - "5514:5514"
      - "8181:8181"
      - "8282:8282"

  raftman-db:
    image: postgres:17-alpine
    restart: unless-stopped
    environment:
      - POSTGRES_USER=raftman
      - POSTGRES_PASSWORD=${RAFTMAN_DB_PASSWORD:-raftman}
      - POSTGRES_DB=raftman
    volumes:
      - raftman-pgdata:/var/lib/postgresql/data

volumes:
  raftman-pgdata:
```

The PostgreSQL port is not published, so the database is reachable only on the
internal Docker network. To switch back to SQLite, drop the `raftman-db`
service and `depends_on`, and set the entrypoint to
`-backend "sqlite:///var/lib/raftman/logs.db?retention=30d"`.

## Notes

- **No automatic data migration.** Switching backends starts with an empty
  store; existing logs are not copied. Because logs are short-lived (bounded by
  `retention`), a fresh start is usually fine.
- **Offline / air-gapped deploys.** The `postgres` image must be bundled
  separately (`docker save` / `docker load`); the SQLite backend needs no extra
  image. raftman itself makes no outbound network calls at runtime.
- **Dependencies.** The PostgreSQL driver (pgx) is pure Go and compiled into the
  binary; no runtime downloads are required.
