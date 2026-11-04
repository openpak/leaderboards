# Protocol: leaderboards

| Client | Surface |
| --- | --- |
| console adapters (`np-score`, later `x360-xstats`, `nex`, `wfc-*`) | gRPC `openpak.leaderboards.v1.Leaderboards` on 20211, `x-internal-key` metadata |
| website, public API, OpenKart, the app | the rankings contract on 20210 (read-only, container network) |
| account-erasure finaliser | `DELETE /internal/accounts/{account}/entries` with `X-Internal-Key` |

## Naming a board

Every gRPC request carries `BoardRef{network, title, board}`:

* `network` is the caller's account-core link namespace: `np`, `x360`, `wiiu`, `switch`, `wfc`, `xbox`.
* `title` is the title as that network knows it (`NPWR04270_00`, an Xbox 360 title id, a Switch
  application id). `title_keys` maps `(network, title)` to the neutral title key; an unmapped id
  creates the title `<network>:<id>` on first write, a read of an unwritten title is empty.
  With `network` empty, `title` is the neutral key itself.
* `board` is a short string (a decimal board number, a course id, a stat view id).

Operators join networks with `MapTitle(title, name, network, native_id)`: one neutral title, one
mapping per network. A mapping that already holds entries under another key refuses to move.
Seeded: `terraria` = PS3 `NPWR04270_00`.

## Policy

| Field | Values | Meaning |
| --- | --- | --- |
| `order` | `desc` (default), `asc` | which end ranks first |
| `aggregation` | `best` (default), `latest`, `sum`, `force` | `best` keeps the better score (ties keep the first); `latest` and `force` (the same, the PS3 name) always replace; `sum` adds |
| `column_aggregations` | column id to `max`, `min`, `sum`, `latest` | per-column merge of `columns` (default `latest`) |
| `score_column` | a column id | the entry's score is that column's merged value (Xbox 360 rating) |
| `kind`, `unit`, `season`, `name` | | display only (`score`, `time`, `rating`) |
| `rank_limit`, `data_limit`, `size_limit` | | adapter-facing; `size_limit` (default 1 MiB) caps game data |
| `cross_network` | true (default) / false | false: each network ranks only the entries it wrote |

`Submit` creates a missing board from `create_policy` (or defaults). `UpsertBoard` is the operator
call and only changes the fields it sets.

## Operations

* `Submit(ref, account, sub, score, columns, comment, info, display_name)` returns `changed`,
  the stored `entry` with `rank` and `position`, the previous score and `total`. `changed=false`
  means the aggregation kept the stored entry (PS3 "not best"). A changed score replaces comment and info
  and drops game data. Limits: comment 512 bytes, info 4096, 64 columns.
* `Range(ref, start, limit)` is by 1-based position (limit default 25, max 100) with `total`.
* `ByAccounts(ref, accounts)` is every entry (all `sub`) of those accounts, best first.
* `FriendRanks(ref, account, include_self, offset, limit)` asks the account core for the friend
  list itself and returns each friend's best entry in rank order with global ranks; `hits` counts
  players found.
* `BoardInfo`, `Total`, `SetData(expect_score)`, `GetData`, `Delete(account[, sub])`,
  `ResetBoard`, `ListBoards`.
* `rank` is the competition rank (equal scores share it); `position` is unique, ordered by
  score, then `updated`, then account, then `sub`. `sub` is an adapter-chosen slot per account
  (PS3 pcId; 0 elsewhere).
* Errors: `InvalidArgument` (bad account, policy), `ResourceExhausted` (too large),
  `FailedPrecondition` (a per-network board read without a network), `NotFound`.

## Cross-network

Entries record the network that wrote them. On the default (cross-network) boards every network
reads the same ranking, so a PS3 score and an Xbox 360 score of Terraria are one board. On a board
with `cross_network=false` the read is filtered to the caller's `network`.

## Events

A new best (first entry or a better score) is announced on the account core's stream as
`leaderboard.record` (`account_id` = the player, `subject_id` = `<title>/<board>`) and, when
`REWARDS_EVENTS_URL`/`KEY` are set, as a rewards event `leaderboard_new_best.<title>` with the
stable id `lb:<title>:<board>:<account>:<updated ms>`. Both are best effort. The rewards service
rejects unknown metrics, so define the metric there before enabling the emitter.

## Rankings contract (website)

The contract in the website repo (`docs/rankings-contract.md`) is served unauthenticated on 20210:

| Base URL | Boards |
| --- | --- |
| `/` | every title's cross-network boards; board id `<title>.<board>`, name `Title: Board` |
| `/t/{title}` | one title (neutral key); board id `<board>` |
| `/n/{network}/t/{title}` | the same, as seen from one network, including its per-network boards |

Append `/internal/rankings/boards` and `/internal/rankings/boards/{id}?limit=&offset=&around=`.
Rows carry `account_id`; only boards holding at least one entry are listed, unless the board list is asked with `?all=1`
(then boards nobody has scored on are listed too; the website uses it for its "no scores yet" view). The website needs no
code change: a `rankingServers` entry for a title is `http://openpak-leaderboards:20210/t/terraria`.

## Store

Postgres `leaderboards`: `titles`, `title_keys`, `boards`, `entries` (primary key
`(title_key, board_id, account, sub)`, index on `(title_key, board_id, score)`). Migrated at start.
