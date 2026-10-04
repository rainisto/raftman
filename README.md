# raftman

![raftman](https://raw.githubusercontent.com/pierredavidbelanger/raftman/master/frontend/static/ui/logo-96.png)

A syslog server with integrated full text search via a JSON API and Web UI.

- [getting started](#getting-started)
- [configuration](#configuration)

## getting started

### store logs

To get started quickly, just run the containerized version of raftman:

```
sudo docker run --rm --name raftman \
    -v /tmp:/var/lib/raftman \
    -p 514:514/udp \
    -p 5514:5514 \
    -p 8181:8181 \
    -p 8282:8282 \
    pierredavidbelanger/raftman
```


This will start raftman with all default options. It listen on port 514 (UDP) and 5514 (TCP) on the host for incoming RFC5424 syslog packets and store them into an SQLite database stored in `/tmp/logs.db` on the host. It also exposes the JSON API on http://localhost:8181/api/ and the Web UI on http://localhost:8282/.

### send logs

Time to fill our database. The easyest way is to just start [logspout](https://github.com/gliderlabs/logspout) and tell it to point to raftman's syslog port:

```
docker run --rm --name logspout \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    --link raftman \
    gliderlabs/logspout \
        syslog://raftman:514
```


This last container will grab other containers output lines and send them as syslog packet to the configured syslog server (ie: our linked raftman container).

### generate logs

Now, we also need to generate some output. This will do the job for now:

```
docker run --rm --name test \
    alpine \
    echo 'Can you see me'
```


### visualise logs

Then we can visualize our logs:

with the raftman API:

```
curl http://localhost:8181/api/list \
    -d '{"Limit": 100, "Message": "see"}'
```


or pop the Web UI at http://localhost:8282/

## configuration

All raftman configuration options are set as arguments in the command line.

For example, here is the what the command line would looks like if we set all the default values explicitly:

```
raftman \
    -backend sqlite:///var/lib/raftman/logs.db?insertQueueSize=512&queryQueueSize=16&timeout=5s&batchSize=32&retention=INF \
    -frontend syslog+udp://:514?format=RFC5424&queueSize=512&timeout=0s \
    -frontend syslog+tcp://:5514?format=RFC5424&queueSize=512&timeout=0s \
    -frontend api+http://:8181/api/ \
    -frontend ui+http://:8282/
```

### backends

raftman can store logs in either an embedded SQLite database (default, no
external dependency) or in an external PostgreSQL server. PostgreSQL is
recommended for large log volumes, where SQLite query latency can become a
bottleneck:

```
# SQLite (default)
raftman sqlite:///var/lib/raftman/logs.db?retention=30d

# PostgreSQL
raftman "postgres://user:pass@host:5432/dbname?sslmode=disable&retention=30d"
```

The JSON API, syslog frontends and Web UI are identical for both backends.
The only user-visible difference is the full text search (`Message`) query
syntax: the SQLite backend uses SQLite FTS4 `MATCH` syntax, while the
PostgreSQL backend uses `websearch_to_tsquery` syntax.

The same `retention`, `batchSize`, `insertQueueSize`, `queryQueueSize` and
`timeout` query parameters are supported by both backends.

### docker-compose with PostgreSQL

```yaml
services:
  raftman:
    image: pierredavidbelanger/raftman
    restart: unless-stopped
    # Exec/list form so the "&" in the URL is passed verbatim (not treated as a
    # shell background operator).
    entrypoint:
      - /usr/local/bin/raftman
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

To use SQLite instead, drop the `raftman-db` service and the `depends_on`, and
set the entrypoint back to `sqlite:///var/lib/raftman/logs.db?retention=30d`.

