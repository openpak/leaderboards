# Leaderboards

OpenPak's neutral leaderboards service.

| | |
| --- | --- |
| Network | Core (all consoles) |
| Role | Boards, scores, per-column stats, comments, info and game data for every network's score server; friend ranking; the website's rankings contract |
| Depends on | [`account`](https://github.com/openpak/account) (friend list, event stream), PostgreSQL |
| Used by | [`np-score`](https://github.com/openpak/np-score) (PS3); later `x360-xstats`, `nex`, `wfc-race`/`wfc-gamestats`; the website (rankings) |
| Image | `ghcr.io/openpak/leaderboards` |
| Ports | 20210 HTTP, 20211 gRPC, 20212 metrics (block 20210) |
| Licence | AGPL-3.0-only |

## Model

* **Title key**: one neutral key per game across networks. `title_keys` maps
  `(network, native id)` to it (a PS3 `NPWR04270_00`, an Xbox 360 title id, a Switch application
  id). An unmapped native id registers itself as `<network>:<native>` so nothing is lost.
* **Board** `(title, board id)`: sort order, aggregation (`best`, `latest`, `sum`, `force` =
  `latest`), per-column aggregation (`max`, `min`, `sum`, `latest`) with an optional score
  column, limits, and `cross_network` (default true; false limits a board to the entries each
  network wrote itself).
* **Entry** `(account, board, sub)`: score, columns, comment, info, game data, the network that
  wrote it, the writer's display name. Accounts are core account ids; adapters map their own ids.

Every request names the caller's network and the title as that network knows it. See
[docs/protocol.md](docs/protocol.md), [docs/adapter-guide.md](docs/adapter-guide.md) and
[docs/deploy.md](docs/deploy.md).

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run ./cmd/leaderboards
```

Tests need PostgreSQL: `LEADERBOARDS_TEST_DATABASE_URL=postgres://… GOWORK=off go test ./...`
(the tests truncate the tables).
