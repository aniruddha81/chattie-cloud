# Chattie Cloud

A horizontally scaled chat system in Go. Several chat instances share one
Postgres database and one Redis; a user connected to any instance can talk to
a user on any other. See [the project plan](Chattie%20Cloud%20—%20Project%20Plan.md)
for the full design and the road to AWS and Azure VMs.

## Run it locally

```
docker compose -f deploy/local/compose.yaml up
```

This pulls the image GitHub Actions built from `main`. Add `--build` to run
the code on your disk instead.

Open http://localhost:8080, create two accounts in two browser windows and
chat. The sidebar shows which instance (`app1`..`app3`) each window landed on.

## Builds

Nothing needs to be built by hand. On every push,
[GitHub Actions](.github/workflows/ci.yml) starts the whole stack, runs the
integration and failure tests against it, and on `main` publishes the Docker
image to `ghcr.io/aniruddha81/chattie-cloud`.

## Run it in the cloud

[docs/deploy-guide.md](docs/deploy-guide.md) deploys the same system to AWS or
Azure VMs with one `terraform apply`: a load balancer, two app VMs and one
data VM, all running the same Docker containers as the local stack.

## How a message travels

1. The browser sends the message over its WebSocket with a `client_message_id`.
2. The instance stores the message and an outbox row in **one Postgres
   transaction**, then acknowledges. Each room hands out its own sequence numbers.
3. The publisher process reads the outbox and announces the event on Redis.
4. Every instance hears the event and forwards the message to its own sockets
   in that room.

Redis delivery is best effort. Clients repair anything they miss by asking
Postgres for "everything after sequence N" when they reconnect, see a gap, get
a `resync` notice, or see a newer sequence in a heartbeat. Retries reuse the
`client_message_id`, so a message is stored once however often it is sent.

## Layout

| Path | What it is |
| --- | --- |
| `cmd/chattie` | The binary: `chattie serve` or `chattie publisher` |
| `internal/api` | HTTP routes, auth cookies, the WebSocket, event fan-out |
| `internal/auth` | Password hashing, signed access tokens, refresh tokens |
| `internal/store/postgres` | All SQL. Postgres is the source of truth |
| `internal/outbox` | Publisher loop: Postgres outbox to Redis |
| `internal/bus` | Redis pub/sub and presence |
| `internal/hub` | The sockets connected to this one instance |
| `migrations` | SQL schema, applied at startup |
| `web` | Browser client, embedded in the binary |
| `deploy/local` | Compose stack: 3 instances, publisher, proxy, Postgres, Redis |
| `deploy/aws`, `deploy/azure` | Terraform for each cloud |
| `deploy/vm` | First-boot scripts the cloud VMs run |
| `tests/integration` | Tests that run against the live stack |
| `tests/load` | Load generator that measures latency |

## Tests

Unit tests need nothing running:

```
go test ./...
```

Integration tests run against the live stack:

```
go test -tags integration ./tests/integration/
```

Failure tests stop containers, so they are opt-in. They cover a Redis outage,
an instance killed while messages are in flight, and a stopped publisher:

```
FAILURE_TESTS=1 go test -tags integration ./tests/integration/
```

In PowerShell, set the variable first: `$env:FAILURE_TESTS = "1"`.

## Load test

```
go run ./tests/load -urls http://localhost:8081,http://localhost:8082 -clients 50 -duration 30s
```

It signs up the clients, spreads them over the instances in rooms of ten,
sends at a steady rate and prints how many messages were delivered and the
latency percentiles. Point `-urls` at a cloud deployment to measure real VMs.

## Security

- Passwords are hashed with bcrypt. Ten failed sign-ins lock a username for 15
  minutes on every instance.
- Sessions are HttpOnly cookies: a 15-minute access token and a refresh token
  that works once.
- State-changing requests from other sites are rejected, and every response
  sets a content security policy that allows scripts and connections only
  from the site itself.
- Not included: HTTPS (it belongs to the load balancer), password reset and
  email verification.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | local Postgres | Postgres connection string |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection string |
| `SESSION_SECRET` | none, required | Signs access tokens; at least 32 characters |
| `COOKIE_SECURE` | `false` | Set `true` when served over HTTPS |
| `LISTEN_ADDR` | `:8080` | Address to listen on |
| `INSTANCE_ID` | hostname | Name shown in logs and to clients |
| `HEARTBEAT_SECONDS` | `20` | Heartbeat and presence renewal interval |
| `DRAIN_SECONDS` | `3` | Time clients get to move away on shutdown |
