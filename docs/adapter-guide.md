# Adapter guide

For the owners of `x360-xstats`/`x360-xarb`, `nex`, `wfc-race`/`wfc-gamestats` and any other
score server. Read [protocol.md](protocol.md) first. The rule (board #40): the adapter keeps the
wire (codecs, ids, signatures, paging shape) and the core keeps the data. Reference adapter:
`np-score` (PS3), which replaced its own table with this service.

## What an adapter does

1. Map the console identity (NpId, XUID, PID, profile id) to a core account id through the
   account core's links. The service only takes account ids.
2. Pick the board: `BoardRef{network, title, board}` with your link namespace, the title id as the
   console knows it, and your board/leaderboard/category id as a string.
3. At start, `UpsertBoard` the boards you know (order, aggregation, limits, names). A board you
   did not declare is created with defaults on first `Submit`. Never keep a score table.
4. Translate calls: submit to `Submit` (`changed=false` is "not a new best"), listing to
   `Range`/`ByAccounts`/`FriendRanks`, game data to `SetData`/`GetData`. Render your own wire from
   the `Entry` (`rank`, `position`, `score`, `columns`, `comment`, `info`, `updated_unix_ms`).
5. Join networks with `MapTitle(neutral key, name, network, native id)` once per title, the same
   neutral key for every network that sells that game. Do it before data exists.
6. Env: `<SERVICE>_LEADERBOARDS_ADDR` (`openpak-leaderboards:20211`) and `..._KEY`
   (`LEADERBOARDS_INTERNAL_KEY`), sent as gRPC metadata `x-internal-key`. Generate the client from
   `proto/openpak/leaderboards/v1/leaderboards.proto` into your own `internal/leaderboardspb`
   (np-score and np-community's chat client are the pattern).

## Per console

### Xbox 360 (`x360-xstats`, `x360-xarb`)

* Leaderboard (stat view) id is the `board`; the title id is `title` with `network = "x360"`.
* Live aggregates per column: declare `column_aggregations` (`sum`, `max`, `min`, `latest`) and
  `score_column` = the rating column. Post the posted columns in `Submit.columns`; the service
  merges them per column and ranks by the merged rating, so `statpost` is one call.
* The special columns (0xFFFF/E/D) and XRL encoding stay in xstats. `x360-xarb` result reports
  `Submit` for each player's account (rather than calling xstats); a stat reset is `ResetBoard`.
* Enumerations by rank, by user and by rating map to `Range`, `ByAccounts` and a rank lookup from
  `Submit.entry.rank`/`ByAccounts`. Write times now exist (`updated_unix_ms`).
* The JSON `X360_XSTATS_STATE` leaderboards migrate with `Submit` per row (use `latest` once so the
  migrated score is stored, since there is no write time).

### NEX (`nex` rankings, ~15 titles)

* NEX ranking category is the `board`; the title is the game's NEX game server id with
  `network = "wiiu"` (3DS: `3ds` shares the PID namespace, pick one and keep it).
* `UploadScore` with a per-category update mode: "all" is `latest`/`force`, "best" is `best`;
  declare it per category with `UpsertBoard`. Ascending (time) categories are `order: "asc"`.
* The ranking `param` goes into `columns` or `info`; the NEX group bytes go into `groups` on
  `Submit` and are read back with `groups_filter` (no adapter-side filtering). `UpdateModeDeleteOld`
  is `Submit` with `aggregation_override: "latest"`. One score per player per category (sub 0);
  common data and ghosts use `SetData`/`GetData` with `sub` 0. Ordinal ranking (1,2,3 with no ties) is `position`,
  standard ranking is `rank`.
* Mario Kart 8 on Wii U and Switch: use the same neutral title key for both and keep
  `cross_network` true to share one board; set it false for a per-network season.

### Wii / DS (`wfc-race`, `wfc-gamestats`)

* Mario Kart Wii courses are boards (`order: "asc"`, `kind: "time"`); the profile id maps to the
  account through the `wfc` link. Ghost data is `SetData` (size limit set on the board).
* GameStats-style per-game tables map to one board per table with the game name as the title.
* Both services already implement the website contract; replace it with the neutral service's by
  deleting their `rankingsapi` copy once their data is in this service, and point the website's
  `rankingServers` entry at `http://openpak-leaderboards:20210/t/<title>`.

## Checklist

* Calls carry `x-internal-key`; the adapter has no database for scores.
* Write tests against a fake leaderboards server (see np-score's `fakeleaderboards`).
* Account erasure needs nothing from you: the finaliser calls
  `DELETE /internal/accounts/{account}/entries`.

## Message sizes

List calls can exceed gRPC's 4 MB default reply size (nx-pctl hit 5 MB for one device against the
parental core). Adapters must raise `MaxCallRecvMsgSize` on the client, or page the list and keep pages
small. Server-side, keep list replies bounded (page size caps) so one account cannot force a huge reply.
