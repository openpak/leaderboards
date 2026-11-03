# Deploy

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run ./cmd/leaderboards
```

## Environment

| Variable | Meaning |
| --- | --- |
| `LEADERBOARDS_DATABASE_URL` | Postgres database `leaderboards`; the schema migrates at start |
| `LEADERBOARDS_HTTP_ADDR` / `_GRPC_ADDR` / `_METRICS_ADDR` | `:20210` / `:20211` / `:20212` |
| `LEADERBOARDS_CORE_ADDRESS`, `LEADERBOARDS_CORE_INTERNAL_KEY` | the account core gRPC and the key presented there |
| `LEADERBOARDS_INTERNAL_KEY` | what adapters (gRPC `x-internal-key`) and the erasure finaliser (`X-Internal-Key`) present |
| `REWARDS_EVENTS_URL`, `REWARDS_EVENTS_KEY[_FILE]` | optional rewards events; off while unset |

## Production (rootless Podman)

1. Create the role and database: `podman exec openpak-postgres psql -U postgres -c 'CREATE DATABASE leaderboards'`.
2. Add the `LEADERBOARDS_*` lines to the shared `~/openpak/deploy/.env`.
3. Copy `deploy/openpak-leaderboards.container` to `~/.config/containers/systemd/`, run
   `~/openpak/deploy/pin-ips.sh --apply`, `systemctl --user daemon-reload`, then
   `systemctl --user start openpak-leaderboards`.
4. Check inside the network: `podman exec openpak-chat wget -qO- http://openpak-leaderboards:20210/healthz`
   (HTTP 200, empty body) and the website contract
   `http://openpak-leaderboards:20210/internal/rankings/boards`.

The HTTP port is on the container network and loopback only; the rankings contract has no key,
like every game server's, so never route it through Traefik.

Image `ghcr.io/openpak/leaderboards`, built by the tag-only release workflow.
