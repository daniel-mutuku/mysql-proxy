# mysql-proxy

A small connection-pooling MySQL proxy written in Go, plus a Docker Compose
lab that shows the problem it solves: **PHP opening more MySQL connections
than the server allows.**

PHP-FPM opens a new MySQL connection for every request. With 50 FPM workers and
a MySQL server capped at 20 connections, a burst of traffic fails with
`SQLSTATE[HY000] [1040] Too many connections`. Put this proxy in between and PHP
can open as many client connections as it likes. The proxy multiplexes them
over a fixed pool of real MySQL connections, and requests that can't get a
connection right away wait in a queue instead of failing.

## Results

1,000 HTTP requests, 50 at a time, against MySQL with `max_connections=20`:

| | Direct to MySQL | Through the proxy |
|---|---:|---:|
| Successful | 225 – 288 | **1000** |
| Failed (`Too many connections`) | 712 – 775 | **0** |
| Requests/sec | ~900 | **~2,500 – 2,700** |
| Peak MySQL connections | 21 (at the limit) | **10** (the pool size) |
| p50 latency | ~6 ms | ~18 ms |
| p99 latency | **~1 s** | ~30 – 48 ms |


How to read these numbers:

- **Direct mode has the lower median latency, but only because so many
  requests fail fast.** A rejected connection returns in a few milliseconds.
  The p99 of about 1 s is PDO's connection retry/back-off when the server
  turns it away.
- **The proxy's median is higher because requests queue.** 50 requests share
  10 backend connections, so most of them wait briefly for one. In exchange,
  every request succeeds and the tail latency drops from about 1 s to about
  30 ms.
- **Throughput is higher through the proxy** for two reasons: no work is
  wasted on failed requests, and the proxy reuses already-authenticated MySQL
  connections, so it skips MySQL's per-connection handshake and auth cost.
- **MySQL never sees more than `-pool-size` connections from the proxy**,
  however many clients connect to the proxy.

---

## Quick start

Requirements: Docker with Compose v2. Go 1.27+ is only needed to run the
benchmark or to run the proxy outside a container.

```bash
cp env.example .env          # then edit the passwords if you like
docker compose up -d --build
```

This starts four containers:

| Container | What it is | Host port |
|---|---|---|
| `mp-mysql` | MySQL 8.4, `max_connections=20`, seeded from `mysql/init.sql` | `3310` |
| `mp-proxy` | The Go proxy (this repo), pool of 10 | `3308` |
| `mp-php` | PHP 8.3-FPM with 50 static workers | — |
| `mp-nginx` | nginx in front of PHP | `8086` (see `APP_HOST_PORT`) |

Check it works:

```bash
curl 'localhost:8086/?mode=proxy'
# {"mode":"proxy","target":"proxy:3308","site_name":"MySQL Proxy Demo","threads_connected":1,"ms":8.01}

curl 'localhost:8086/?mode=direct'
# {"mode":"direct","target":"mysql:3306","site_name":"MySQL Proxy Demo","threads_connected":1,"ms":3.2}
```

Run the comparison:

```bash
cd proxy
go run ./cmd/bench -mode direct -c 50 -n 1000
go run ./cmd/bench -mode proxy  -c 50 -n 1000
```

Watch the pool while the benchmark runs:

```bash
docker logs -f mp-proxy
# clients=50 backend: in_use=10 idle=0 waiting=40 (dialled 10 total)
```


Tear down (add `-v` to also delete the MySQL data volume):

```bash
docker compose down
```

---


## How the proxy works

The proxy speaks the MySQL wire protocol on both sides, using
[go-mysql](https://github.com/go-mysql-org/go-mysql). It uses `server` for the
client-facing side and `client` for the MySQL side.

### 1. Accepting clients

`main.go` accepts TCP connections and runs a goroutine per client. Each one
does the MySQL handshake with `server.NewDefaultServer()` and checks the
client's username and password against `PROXY_USER` / `PROXY_PASS`.

**No MySQL connection is opened at this point.** A connected client costs one
goroutine and one socket, and nothing on the MySQL side.

### 2. Borrowing a backend connection per command

Each client gets a `session` (`handler.go`). When a command arrives (a query,
`USE db`, a prepare, and so on), the session:

1. **acquires** a backend connection from the pool,
2. switches it to the client's current database if it is on a different one,
3. runs the command,
4. **releases** the connection back to the pool, unless it has to stay pinned
   (see below).

So a backend connection is held only for the duration of one statement, not
for the life of the client connection. This is *statement-level pooling*,
roughly what PgBouncer calls transaction pooling.

The session remembers the client's selected database, whether it came from the
handshake, `COM_INIT_DB` or a plain `USE dbname` query. That way whichever
backend connection it gets next can be switched to the right schema.

### 3. The pool (`pool.go`)

- `slots` is a buffered channel of capacity `-pool-size`. Holding a token
  means you are allowed to hold a backend connection. This is what caps MySQL
  connections at `-pool-size`.
- `idle` is a buffered channel of open connections that nobody is using.
- `get()` waits for a slot for up to `-pool-wait`. It then reuses an idle
  connection if there is one, or dials a new one. If no slot frees up in time,
  the client gets MySQL error **1040** with the message
  `proxy: no backend connection free within 5s (pool size 10)`.
- `put(conn, reusable)` returns the connection to `idle`, or closes it if it
  can't be reused, and frees the slot either way.
- A connection that has been idle for more than **30 s** is pinged before it
  is handed out. If the ping fails (for example MySQL's `wait_timeout` dropped
  it), it is closed and the pool tries the next one.
- Connections are dialled lazily. The pool starts empty and grows to
  `-pool-size` only under load.

### 4. When a connection is pinned

Some MySQL state lives on the connection itself. If the proxy gave that
connection to another client in the middle of it, things would break. So the
session keeps its backend connection (it is *pinned*) while any of these are
true:

| Condition | Why | Unpinned when |
|---|---|---|
| A transaction is open (`BEGIN`, `autocommit=0`, …) | Later statements must run in the same transaction | `COMMIT` / `ROLLBACK` |
| A server-side prepared statement is open | Statement IDs belong to the connection | Every statement is closed |
| The session ran `SET …`, `LOCK …`, `CREATE TEMPORARY …`, `PREPARE …`, `GET_LOCK(` or `SQL_CALC_FOUND_ROWS` | The effect outlives the statement (session variables, table locks, temp tables, named locks, `FOUND_ROWS()`) | Never. It stays *sticky* until the client disconnects |

The sticky check is a deliberately simple prefix/substring match. Being wrong
in the "pin it" direction only costs pooling efficiency, never correctness.

When a client disconnects while still holding a connection, the proxy
**closes** that connection instead of returning it to the pool, because it may
carry session state the next client must not inherit.

PHP's `pdo_mysql` emulates prepared statements by default, so the demo app
sends plain queries and never pins a connection. Clients that use real
server-side prepares (for example `PDO::ATTR_EMULATE_PREPARES => false`) pin a
connection for as long as their statements stay open.

### 5. Errors

- **MySQL errors** (a syntax error, a duplicate key, …) are passed through
  unchanged, and the connection stays reusable.
- **Any other error** (network failure, protocol error) means the backend
  connection is suspect. It is closed instead of being returned to the pool.

### 6. Graceful shutdown

On `SIGINT` or `SIGTERM` (`docker stop` sends `SIGTERM`), the proxy:

1. stops accepting new clients,
2. waits up to `-grace` (default 10 s) for connected clients to finish and
   disconnect,
3. force-closes any that are still connected,
4. closes the idle backend connections and exits.

A second Ctrl+C kills the process immediately. In Compose,
`stop_grace_period: 15s` gives the proxy longer than its own 10 s grace before
Docker sends `SIGKILL`. The container runs the binary directly as PID 1, with
no shell wrapper, so the signal reaches the proxy.

### 7. Stats logging

Every `-stats` interval (default 2 s), the proxy logs a line, but only when
the numbers have changed since the last one:

```
clients=50 backend: in_use=10 idle=0 waiting=40 (dialled 10 total)
```

| Field | Meaning |
|---|---|
| `clients` | Client connections currently open to the proxy |
| `in_use` | Pool slots taken (backend connections lent out) |
| `idle` | Open backend connections waiting to be reused |
| `waiting` | Commands blocked waiting for a slot |
| `dialled` | Backend connections opened since startup. If this stays flat under load, reuse is working |

---

## Configuration

### Proxy flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:3308` | Address to listen on |
| `-pool-size` | `10` | Maximum backend MySQL connections the proxy will open |
| `-pool-wait` | `5s` | How long a command waits for a free backend connection before failing with error 1040 |
| `-grace` | `10s` | How long shutdown waits for clients to disconnect before closing them |
| `-stats` | `2s` | How often to log pool stats (`0` turns it off) |
| `-debug` | `false` | Log every query and every connect/disconnect |

### Proxy environment variables (all required except `BACKEND_PASS`)

| Variable | Description |
|---|---|
| `PROXY_USER` / `PROXY_PASS` | Credentials clients must use to log in to the proxy |
| `BACKEND_ADDR` | MySQL `host:port` the proxy connects to |
| `BACKEND_USER` / `BACKEND_PASS` | Credentials the proxy uses for every backend connection |

In Compose, both credential pairs are set from `DB_USER` / `DB_PASS`, so PHP
uses the same login whichever mode it is in.

### `.env` (Compose)

| Variable | Example | Used by |
|---|---|---|
| `MYSQL_VERSION` | `8.4` | mysql image tag |
| `MYSQL_ROOT_PASSWORD` | `rootpass` | mysql |
| `MYSQL_HOST_PORT` | `3310` | MySQL port on the host |
| `MYSQL_MAX_CONNECTIONS` | `20` | MySQL `--max-connections`. Kept low on purpose to reproduce the failure |
| `DB_NAME` / `DB_USER` / `DB_PASS` | `appdb` / `app` / `apppass` | App database and user. Also the proxy's client and backend credentials |
| `DIRECT_DB_HOST` / `DIRECT_DB_PORT` | `mysql` / `3306` | Where PHP connects in `?mode=direct` |
| `PROXY_DB_HOST` / `PROXY_DB_PORT` | `proxy` / `3308` | Where PHP connects in `?mode=proxy` |
| `PROXY_POOL_SIZE` | `10` | Proxy `-pool-size`. Keep it below `MYSQL_MAX_CONNECTIONS` |
| `PROXY_POOL_WAIT` | `5s` | Proxy `-pool-wait` |
| `PROXY_HOST_PORT` | `3308` | Proxy port on the host |
| `APP_HOST_PORT` | `8086` | nginx port on the host |

After changing `.env`, apply it with `docker compose up -d`. Compose recreates
only the containers whose configuration changed.

### Sizing the pool

- `PROXY_POOL_SIZE` must be **below** `MYSQL_MAX_CONNECTIONS`, leaving room
  for direct connections, admin sessions and replication. If they are equal,
  anything else that connects to MySQL can starve the proxy.
- A bigger pool doesn't automatically mean more throughput. Once MySQL's CPUs
  or disks are saturated, extra connections only add contention. Try a few
  sizes with the benchmark and see where requests/sec stops improving.
- If you see `proxy: no backend connection free within …` errors, the pool is
  saturated for longer than `-pool-wait`. Either increase `-pool-wait` to
  absorb bursts, increase `-pool-size` if MySQL has headroom, or look for
  clients pinning connections (long transactions, sticky `SET` statements).

---

## Running the proxy outside Docker

Useful while you are changing the proxy code, since there is no image rebuild
on each edit.

```bash
docker compose stop proxy          # free port 3308

cd proxy
PROXY_USER=app PROXY_PASS=apppass \
BACKEND_ADDR=127.0.0.1:3310 BACKEND_USER=app BACKEND_PASS=apppass \
go run ./cmd/server -pool-size 10 -debug
```

Then point PHP at the host, in `.env`:

```bash
PROXY_DB_HOST=host.docker.internal
```

and recreate PHP with `docker compose up -d php`. The `php` service already
maps `host.docker.internal` to the host gateway.

To go back to the container, set `PROXY_DB_HOST=proxy` and run
`docker compose up -d`.

Rebuild the image after code changes:

```bash
docker compose up -d --build proxy
```

---

## The benchmark tool

`proxy/cmd/bench` sends HTTP requests to the demo app with a fixed number of
concurrent workers and summarises the results.

```bash
go run ./cmd/bench [-mode direct|proxy] [-c N] [-n N] [-url URL] [-t TIMEOUT]
```

| Flag | Default | Description |
|---|---|---|
| `-mode` | `proxy` | Added to the URL as `?mode=` |
| `-c` | `1` | Concurrent workers |
| `-n` | same as `-c` | Total requests |
| `-url` | `http://localhost:8086` | Base URL of the app |
| `-t` | `10s` | Per-request timeout |

It reports total time, requests/sec, success and failure counts, the highest
`Threads_connected` any successful response saw ("Max MySQL threads"),
min/p50/p95/p99/max latency, and failures grouped by error message. Ctrl+C
stops it early and reports on the requests that completed. A second Ctrl+C
quits immediately.

Things to try:

```bash
# Fewer workers than MySQL allows: direct mode is fine
go run ./cmd/bench -mode direct -c 10 -n 500

# Shrink the pool to see queueing (PROXY_POOL_SIZE=2 in .env, then docker compose up -d)
go run ./cmd/bench -mode proxy -c 50 -n 1000

# Make the wait budget too small to see the proxy's own 1040 error (PROXY_POOL_WAIT=1ms)
go run ./cmd/bench -mode proxy -c 50 -n 1000
```

---

## Talking to the proxy directly

Any MySQL client works:

```bash
mysql -h 127.0.0.1 -P 3308 -u app -p appdb
```

```sql
SELECT * FROM settings;
SHOW STATUS LIKE 'Threads_connected';   -- counted on MySQL, not the proxy
```

Run with `-debug` (add `- -debug` under `command:` in `docker-compose.yml`) to
log every query the proxy receives.

---