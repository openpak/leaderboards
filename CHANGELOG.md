# Changelog — leaderboards

`git log` stays the source of truth; this file is the readable summary.

## v0.2.3 — 2026-11-04
- `GET .../internal/rankings/boards?all=1` also lists boards with no entries yet (default unchanged), for the website's every-game rankings hub.

## v0.2.2 — 2026-11-04
- `/n/{network}/t/{title}`: the title segment is resolved as a native id on that network (`ResolveTitle`, no create) and falls back to the raw
  value, so `/n/np/t/NPWR…` and `/n/x360/t/<hex>` serve the mapped title's boards instead of an empty list. Test added.

## v0.2.1 — 2026-11-03
- GET .../internal/rankings/boards (constant poll) is counted and summarised once a minute instead of logged per call; failures still log.

## v0.2.0 — 2026-11-03
- Per-request logging: one slog line per HTTP request (method, path, status, ms) and per gRPC unary call (method, code, ms) on stderr via internal/httplog. Query strings, headers and bodies are never logged; /healthz and /metrics are skipped.

## v0.1.2 — 2026-11-03

- Adapter guide: gRPC message-size note (raise MaxCallRecvMsgSize or page lists).

## v0.1.1 — 2026-11-03

- Docs: website wiring (health route, erasure step).

## v0.1.0 — 2026-11-03

- First release: the neutral board store (board #51, parent #40). Titles are keyed the same on
  every network (`title_keys` maps `(network, native id)`, seeded for Terraria PS3
  `NPWR04270_00`); boards carry a policy (order, aggregation best/latest/sum/force, per-column
  aggregation and a score column for Xbox 360 stats, limits, cross-network flag); entries are
  `(account, board, sub)` with score, columns, comment, info, game data and the source network.
- gRPC `openpak.leaderboards.v1.Leaderboards` on 20211: Submit, Range, ByAccounts, FriendRanks
  (friends from the account core), BoardInfo, Total, SetData, GetData, Delete, UpsertBoard,
  MapTitle, ResetBoard, ListBoards.
- The website rankings contract served natively on 20210: `/internal/rankings/*` (all titles),
  `/t/{title}/internal/rankings/*` (one title, usable as the website's base URL) and
  `/n/{network}/t/{title}/...` (one network's view of per-network boards).
- `DELETE /internal/accounts/{account}/entries` for account erasure.
- A new best is announced on the account core's stream (`leaderboard.record`) and, when
  `REWARDS_EVENTS_URL`/`KEY` are set, as a rewards event.
