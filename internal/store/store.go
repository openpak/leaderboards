// Package store holds the leaderboards: titles and their per-network native ids, boards with
// their policy, and the entries. One Postgres database (`leaderboards`); the schema migrates at
// start. Nothing console-shaped is stored: adapters map their ids to core accounts first.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var keyChars = regexp.MustCompile(`[^A-Za-z0-9:_-]`)

var (
	ErrNoBoard   = errors.New("leaderboards: no such board")
	ErrNoTitle   = errors.New("leaderboards: unknown title")
	ErrBadPolicy = errors.New("leaderboards: invalid policy")
	ErrConflict  = errors.New("leaderboards: title mapping conflict")
)

// Aggregations a board can use for its score, and the per-column ones.
const (
	AggBest   = "best"
	AggLatest = "latest"
	AggSum    = "sum"
	AggForce  = "force" // stored as latest
)

// Defaults for a board nobody configured.
const (
	DefaultSizeLimit = 1 << 20
	DefaultDataLimit = 10
	DefaultRankLimit = 100000
)

// Board is one board's resolved rules.
type Board struct {
	Title       string
	ID          string
	Name        string
	Kind        string
	Order       string // asc | desc
	Aggregation string
	ColumnAggs  map[string]string
	ScoreColumn string
	Unit        string
	Season      string
	RankLimit   int
	DataLimit   int
	SizeLimit   int
	Cross       bool
	Exists      bool
}

func (b Board) Asc() bool { return b.Order == "asc" }

// Entry is one stored row plus its computed place.
type Entry struct {
	Account     string
	Sub         int
	Score       int64
	Columns     map[string]int64
	Comment     string
	Info        []byte
	Groups      []byte // optional per-entry group bytes (NEX ranking groups); filterable
	HasData     bool
	Network     string
	DisplayName string
	Updated     time.Time
	Rank        int64 // competition rank: ties share it
	Position    int64 // unique 1-based position
}

// Group filters a read to entries whose Groups byte at Index equals Value. Entries with no such
// byte (shorter or empty groups) never match; a Value above 255 matches nothing. nil = no filter.
type Group struct{ Index, Value int }

// SubmitOpts are the optional parts of a Submit.
type SubmitOpts struct {
	Groups      []byte // stored with the score (score-bound, like info)
	AggOverride string // "latest": this call replaces the stored entry whatever the board aggregation
}

type Store struct{ DB *pgxpool.Pool }

func (g *Group) args() (int, int) {
	if g == nil {
		return -1, 0
	}
	return g.Index, g.Value
}

const schema = `
CREATE TABLE IF NOT EXISTS titles (
    key  TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS title_keys (
    network   TEXT NOT NULL,
    native_id TEXT NOT NULL,
    title_key TEXT NOT NULL REFERENCES titles(key) ON UPDATE CASCADE,
    PRIMARY KEY (network, native_id)
);
CREATE TABLE IF NOT EXISTS boards (
    title_key    TEXT NOT NULL REFERENCES titles(key) ON UPDATE CASCADE,
    board_id     TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'score',
    sort_order   TEXT NOT NULL DEFAULT 'desc' CHECK (sort_order IN ('asc','desc')),
    aggregation  TEXT NOT NULL DEFAULT 'best' CHECK (aggregation IN ('best','latest','sum')),
    column_aggs  JSONB NOT NULL DEFAULT '{}',
    score_column TEXT NOT NULL DEFAULT '',
    unit         TEXT NOT NULL DEFAULT '',
    season       TEXT NOT NULL DEFAULT '',
    rank_limit   INTEGER NOT NULL DEFAULT 100000,
    data_limit   INTEGER NOT NULL DEFAULT 10,
    size_limit   INTEGER NOT NULL DEFAULT 1048576,
    cross_network BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (title_key, board_id)
);
CREATE TABLE IF NOT EXISTS entries (
    title_key    TEXT NOT NULL,
    board_id     TEXT NOT NULL,
    account      UUID NOT NULL,
    sub          INTEGER NOT NULL DEFAULT 0,
    score        BIGINT NOT NULL,
    columns      JSONB NOT NULL DEFAULT '{}',
    comment      TEXT NOT NULL DEFAULT '',
    info         BYTEA NOT NULL DEFAULT ''::bytea,
    groups       BYTEA NOT NULL DEFAULT ''::bytea,
    data         BYTEA,
    network      TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    updated      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (title_key, board_id, account, sub)
);
ALTER TABLE entries ADD COLUMN IF NOT EXISTS groups BYTEA NOT NULL DEFAULT ''::bytea;
CREATE INDEX IF NOT EXISTS entries_rank ON entries (title_key, board_id, score);
CREATE INDEX IF NOT EXISTS entries_account ON entries (account);

INSERT INTO titles (key, name) VALUES ('terraria', 'Terraria') ON CONFLICT DO NOTHING;
INSERT INTO title_keys (network, native_id, title_key) VALUES ('np', 'NPWR04270_00', 'terraria') ON CONFLICT DO NOTHING;
`

// Open opens the pool and migrates.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{DB: pool}, nil
}

func (s *Store) Close() { s.DB.Close() }

// ResolveTitle maps (network, native id) to the neutral title key. With create, an unmapped
// native id registers itself as title "<network>:<native>" so nothing is lost; the operator can
// MapTitle it before data exists. With an empty network, native is already a title key.
func (s *Store) ResolveTitle(ctx context.Context, network, native string, create bool) (string, error) {
	native = strings.TrimSpace(native)
	if native == "" {
		return "", ErrNoTitle
	}
	if network == "" {
		var k string
		err := s.DB.QueryRow(ctx, `SELECT key FROM titles WHERE key = $1`, native).Scan(&k)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoTitle
		}
		return k, err
	}
	var k string
	err := s.DB.QueryRow(ctx, `SELECT title_key FROM title_keys WHERE network = $1 AND native_id = $2`, network, native).Scan(&k)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if !create {
		return "", ErrNoTitle
	}
	key := network + ":" + keyChars.ReplaceAllString(native, "_")
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO titles (key, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, key, native); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO title_keys (network, native_id, title_key) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, network, native, key); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	// A concurrent MapTitle may have won.
	err = s.DB.QueryRow(ctx, `SELECT title_key FROM title_keys WHERE network = $1 AND native_id = $2`, network, native).Scan(&k)
	return k, err
}

// MapTitle creates the title if needed and maps (network, native) to it. A mapping that already
// points elsewhere is moved only when nothing has been stored under the old key.
func (s *Store) MapTitle(ctx context.Context, title, name, network, native string) error {
	if title == "" {
		return ErrNoTitle
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO titles (key, name) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE titles.name END`, title, name); err != nil {
		return err
	}
	if network != "" && native != "" {
		var old string
		err = tx.QueryRow(ctx, `SELECT title_key FROM title_keys WHERE network = $1 AND native_id = $2 FOR UPDATE`, network, native).Scan(&old)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `INSERT INTO title_keys (network, native_id, title_key) VALUES ($1, $2, $3)`, network, native, title)
		case err != nil:
		case old != title:
			var n int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM entries WHERE title_key = $1`, old).Scan(&n); err == nil {
				if n > 0 {
					return fmt.Errorf("%w: %s/%s already holds %d entries under %q", ErrConflict, network, native, n, old)
				}
				_, err = tx.Exec(ctx, `UPDATE title_keys SET title_key = $3 WHERE network = $1 AND native_id = $2`, network, native, title)
			}
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Policy is what UpsertBoard / board creation carries; zero fields keep the stored value.
type Policy struct {
	Name, Kind, Order, Aggregation, ScoreColumn, Unit, Season string
	ColumnAggs                                                map[string]string
	RankLimit, DataLimit, SizeLimit                           int
	Cross                                                     *bool
}

func normAgg(a string) (string, error) {
	switch a {
	case "", AggBest:
		return AggBest, nil
	case AggLatest, AggForce:
		return AggLatest, nil
	case AggSum:
		return AggSum, nil
	}
	return "", ErrBadPolicy
}

func validColumnAgg(a string) bool {
	switch a {
	case "max", "min", "sum", "latest":
		return true
	}
	return false
}

// Validate checks the pieces of a policy that have a closed set of values.
func (p Policy) Validate() error {
	if p.Order != "" && p.Order != "asc" && p.Order != "desc" {
		return ErrBadPolicy
	}
	if p.Aggregation != "" {
		if _, err := normAgg(p.Aggregation); err != nil {
			return err
		}
	}
	for _, a := range p.ColumnAggs {
		if !validColumnAgg(a) {
			return ErrBadPolicy
		}
	}
	return nil
}

const boardCols = `name, kind, sort_order, aggregation, column_aggs, score_column, unit, season, rank_limit, data_limit, size_limit, cross_network`

func scanBoard(row pgx.Row, b *Board) error {
	var aggs []byte
	if err := row.Scan(&b.Name, &b.Kind, &b.Order, &b.Aggregation, &aggs, &b.ScoreColumn, &b.Unit, &b.Season, &b.RankLimit, &b.DataLimit, &b.SizeLimit, &b.Cross); err != nil {
		return err
	}
	b.ColumnAggs = map[string]string{}
	return json.Unmarshal(aggs, &b.ColumnAggs)
}

func defaultBoard(title, id string) Board {
	return Board{Title: title, ID: id, Kind: "score", Order: "desc", Aggregation: AggBest, ColumnAggs: map[string]string{},
		RankLimit: DefaultRankLimit, DataLimit: DefaultDataLimit, SizeLimit: DefaultSizeLimit, Cross: true}
}

// Board loads a board; a board nobody configured comes back with defaults and Exists=false.
func (s *Store) Board(ctx context.Context, title, id string) (Board, error) {
	b := defaultBoard(title, id)
	err := scanBoard(s.DB.QueryRow(ctx, `SELECT `+boardCols+` FROM boards WHERE title_key = $1 AND board_id = $2`, title, id), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultBoard(title, id), nil
	}
	b.Exists = err == nil
	return b, err
}

// UpsertBoard creates the board or updates the fields the policy sets.
func (s *Store) UpsertBoard(ctx context.Context, title, id string, p Policy) (Board, error) {
	if err := p.Validate(); err != nil {
		return Board{}, err
	}
	cur, err := s.Board(ctx, title, id)
	if err != nil {
		return Board{}, err
	}
	b := cur
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&b.Name, p.Name)
	set(&b.Kind, p.Kind)
	set(&b.Order, p.Order)
	set(&b.ScoreColumn, p.ScoreColumn)
	set(&b.Unit, p.Unit)
	set(&b.Season, p.Season)
	if p.Aggregation != "" {
		b.Aggregation, _ = normAgg(p.Aggregation)
	}
	if p.ColumnAggs != nil {
		b.ColumnAggs = p.ColumnAggs
	}
	if p.RankLimit > 0 {
		b.RankLimit = p.RankLimit
	}
	if p.DataLimit > 0 {
		b.DataLimit = p.DataLimit
	}
	if p.SizeLimit > 0 {
		b.SizeLimit = p.SizeLimit
	}
	if p.Cross != nil {
		b.Cross = *p.Cross
	}
	aggs, _ := json.Marshal(b.ColumnAggs)
	_, err = s.DB.Exec(ctx, `INSERT INTO boards (title_key, board_id, name, kind, sort_order, aggregation, column_aggs, score_column, unit, season, rank_limit, data_limit, size_limit, cross_network)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (title_key, board_id) DO UPDATE SET name=$3, kind=$4, sort_order=$5, aggregation=$6, column_aggs=$7,
		score_column=$8, unit=$9, season=$10, rank_limit=$11, data_limit=$12, size_limit=$13, cross_network=$14`,
		title, id, b.Name, b.Kind, b.Order, b.Aggregation, aggs, b.ScoreColumn, b.Unit, b.Season, b.RankLimit, b.DataLimit, b.SizeLimit, b.Cross)
	b.Exists = true
	return b, err
}

// EnsureBoard returns the board, creating it from p (or defaults) when it does not exist.
func (s *Store) EnsureBoard(ctx context.Context, title, id string, p Policy) (Board, error) {
	b, err := s.Board(ctx, title, id)
	if err != nil || b.Exists {
		return b, err
	}
	return s.UpsertBoard(ctx, title, id, p)
}

// netFilter is the network the ranking is restricted to, "" for everyone.
func netFilter(b Board, network string) string {
	if b.Cross {
		return ""
	}
	return network
}

func dir(asc bool) string {
	if asc {
		return "ASC"
	}
	return "DESC"
}

func ranked(asc bool) string {
	d := dir(asc)
	// $4/$5 are the groups filter (index -1 = none). Ranks and positions are among the filtered rows.
	return `WITH r AS (SELECT account::text, sub, score, columns, comment, info, groups, data IS NOT NULL AS has_data, network, display_name, updated,
		rank() OVER (ORDER BY score ` + d + `) AS rnk, row_number() OVER (ORDER BY score ` + d + `, updated, account, sub) AS pos
		FROM entries WHERE title_key = $1 AND board_id = $2 AND ($3 = '' OR network = $3)
		AND (CASE WHEN $4 < 0 THEN true WHEN length(groups) > $4 THEN get_byte(groups, $4) = $5 ELSE false END)) SELECT * FROM r `
}

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var cols []byte
		if err := rows.Scan(&e.Account, &e.Sub, &e.Score, &cols, &e.Comment, &e.Info, &e.Groups, &e.HasData, &e.Network, &e.DisplayName, &e.Updated, &e.Rank, &e.Position); err != nil {
			return nil, err
		}
		e.Columns = map[string]int64{}
		if err := json.Unmarshal(cols, &e.Columns); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Total(ctx context.Context, b Board, network string, g *Group) (int64, error) {
	var n int64
	gi, gv := g.args()
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM entries WHERE title_key = $1 AND board_id = $2 AND ($3 = '' OR network = $3)
		AND (CASE WHEN $4 < 0 THEN true WHEN length(groups) > $4 THEN get_byte(groups, $4) = $5 ELSE false END)`,
		b.Title, b.ID, netFilter(b, network), gi, gv).Scan(&n)
	return n, err
}

// Range returns up to n entries from 1-based position start.
func (s *Store) Range(ctx context.Context, b Board, network string, start, n int64, g *Group) ([]Entry, error) {
	gi, gv := g.args()
	rows, err := s.DB.Query(ctx, ranked(b.Asc())+`WHERE pos >= $6 ORDER BY pos LIMIT $7`, b.Title, b.ID, netFilter(b, network), gi, gv, start, n)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// Accounts returns every entry of the given accounts, best first.
func (s *Store) Accounts(ctx context.Context, b Board, network string, accounts []string, g *Group) ([]Entry, error) {
	gi, gv := g.args()
	rows, err := s.DB.Query(ctx, ranked(b.Asc())+`WHERE account = ANY($6::text[]) ORDER BY pos`, b.Title, b.ID, netFilter(b, network), gi, gv, accounts)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// SubmitResult is what Submit stored.
type SubmitResult struct {
	Changed     bool
	Entry       Entry
	Previous    int64
	HadPrevious bool
	Improved    bool // new entry or a better score: the case worth announcing
}

func better(asc bool, n, cur int64) bool {
	if asc {
		return n < cur
	}
	return n > cur
}

func mergeColumn(agg string, asc bool, cur, n int64) int64 {
	switch agg {
	case "sum":
		return cur + n
	case "max":
		return max(cur, n)
	case "min":
		return min(cur, n)
	}
	return n
}

func sameColumns(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Submit applies the board's aggregation to (account, sub) atomically.
func (s *Store) Submit(ctx context.Context, b Board, network, account string, sub int, score int64, cols map[string]int64, comment string, info []byte, display string, opt SubmitOpts) (SubmitResult, error) {
	if opt.AggOverride != "" {
		agg, err := normAgg(opt.AggOverride)
		if err != nil || agg != AggLatest {
			return SubmitResult{}, ErrBadPolicy // only "latest" (alias "force") may be forced per call
		}
		b.Aggregation = AggLatest
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return SubmitResult{}, err
	}
	defer tx.Rollback(ctx)
	var res SubmitResult
	var cur int64
	var curColsRaw []byte
	err = tx.QueryRow(ctx, `SELECT score, columns FROM entries WHERE title_key=$1 AND board_id=$2 AND account=$3 AND sub=$4 FOR UPDATE`,
		b.Title, b.ID, account, sub).Scan(&cur, &curColsRaw)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	curCols := map[string]int64{}
	if exists {
		if err = json.Unmarshal(curColsRaw, &curCols); err != nil {
			return res, err
		}
		res.Previous, res.HadPrevious = cur, true
	}
	merged := make(map[string]int64, len(curCols)+len(cols))
	for k, v := range curCols {
		merged[k] = v
	}
	for k, v := range cols {
		if old, ok := curCols[k]; ok {
			agg := b.ColumnAggs[k]
			if agg == "" {
				agg = "latest"
			}
			merged[k] = mergeColumn(agg, b.Asc(), old, v)
		} else {
			merged[k] = v
		}
	}
	newScore := score
	if b.ScoreColumn != "" {
		if v, ok := merged[b.ScoreColumn]; ok {
			newScore = v
		} else {
			newScore = cur
		}
	}
	scoreWrites := true // the score-bound fields (comment, info, data) are replaced
	if exists {
		switch {
		case b.ScoreColumn != "":
			scoreWrites = newScore != cur
		case b.Aggregation == AggBest:
			scoreWrites = better(b.Asc(), newScore, cur)
			if !scoreWrites {
				newScore = cur
			}
		case b.Aggregation == AggSum:
			newScore = cur + score
		}
	}
	colsChanged := !exists || !sameColumns(curCols, merged)
	res.Improved = !exists || better(b.Asc(), newScore, cur)
	switch {
	case !exists || scoreWrites:
		colsJSON, _ := json.Marshal(merged)
		if info == nil {
			info = []byte{}
		}
		groups := opt.Groups
		if groups == nil {
			groups = []byte{}
		}
		_, err = tx.Exec(ctx, `INSERT INTO entries (title_key, board_id, account, sub, score, columns, comment, info, groups, data, network, display_name, updated)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$11,NULL,$9,$10,now())
			ON CONFLICT (title_key, board_id, account, sub) DO UPDATE SET score=$5, columns=$6, comment=$7, info=$8, groups=$11, data=NULL, network=$9, display_name=$10, updated=now()`,
			b.Title, b.ID, account, sub, newScore, colsJSON, comment, info, network, display, groups)
		res.Changed = true
	case colsChanged:
		colsJSON, _ := json.Marshal(merged)
		_, err = tx.Exec(ctx, `UPDATE entries SET columns=$5 WHERE title_key=$1 AND board_id=$2 AND account=$3 AND sub=$4`, b.Title, b.ID, account, sub, colsJSON)
		res.Changed = true
	}
	if err != nil {
		return res, err
	}
	if err = tx.Commit(ctx); err != nil {
		return res, err
	}
	res.Entry, err = s.One(ctx, b, network, account, sub)
	return res, err
}

// One returns an entry with its place, ErrNoBoard-free: pgx.ErrNoRows when absent.
func (s *Store) One(ctx context.Context, b Board, network, account string, sub int) (Entry, error) {
	rows, err := s.DB.Query(ctx, ranked(b.Asc())+`WHERE account = $6 AND sub = $7`, b.Title, b.ID, netFilter(b, network), -1, 0, account, sub)
	if err != nil {
		return Entry{}, err
	}
	es, err := scanEntries(rows)
	if err != nil {
		return Entry{}, err
	}
	if len(es) == 0 {
		// Cross-network off and the entry was written from another network: report it unranked.
		return Entry{Account: account, Sub: sub}, pgx.ErrNoRows
	}
	return es[0], nil
}

// SetData attaches data to the entry holding exactly score.
func (s *Store) SetData(ctx context.Context, b Board, account string, sub int, score int64, data []byte) (bool, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE entries SET data = $6 WHERE title_key=$1 AND board_id=$2 AND account=$3 AND sub=$4 AND score=$5`,
		b.Title, b.ID, account, sub, score, data)
	return tag.RowsAffected() == 1, err
}

// Data returns an entry's data, nil when absent.
func (s *Store) Data(ctx context.Context, b Board, account string, sub int) ([]byte, error) {
	var data []byte
	err := s.DB.QueryRow(ctx, `SELECT data FROM entries WHERE title_key=$1 AND board_id=$2 AND account=$3 AND sub=$4`, b.Title, b.ID, account, sub).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

// Delete removes an account's entries on a board (one sub, or all).
func (s *Store) Delete(ctx context.Context, b Board, account string, oneSub bool, sub int) (int64, error) {
	q := `DELETE FROM entries WHERE title_key=$1 AND board_id=$2 AND account=$3`
	args := []any{b.Title, b.ID, account}
	if oneSub {
		q += ` AND sub=$4`
		args = append(args, sub)
	}
	tag, err := s.DB.Exec(ctx, q, args...)
	return tag.RowsAffected(), err
}

// EraseAccount removes every entry of the account on every board.
func (s *Store) EraseAccount(ctx context.Context, account string) (int64, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM entries WHERE account = $1`, account)
	return tag.RowsAffected(), err
}

func (s *Store) Reset(ctx context.Context, b Board) (int64, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM entries WHERE title_key=$1 AND board_id=$2`, b.Title, b.ID)
	return tag.RowsAffected(), err
}

// Boards lists configured boards of a title, or of every title when title is "".
func (s *Store) Boards(ctx context.Context, title string, onlyWithEntries bool) ([]Board, error) {
	q := `SELECT title_key, board_id, ` + boardCols + ` FROM boards b WHERE ($1 = '' OR title_key = $1)`
	if onlyWithEntries {
		q += ` AND EXISTS (SELECT 1 FROM entries e WHERE e.title_key = b.title_key AND e.board_id = b.board_id)`
	}
	rows, err := s.DB.Query(ctx, q+` ORDER BY title_key, board_id`, title)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Board
	for rows.Next() {
		var b Board
		var aggs []byte
		if err := rows.Scan(&b.Title, &b.ID, &b.Name, &b.Kind, &b.Order, &b.Aggregation, &aggs, &b.ScoreColumn, &b.Unit, &b.Season, &b.RankLimit, &b.DataLimit, &b.SizeLimit, &b.Cross); err != nil {
			return nil, err
		}
		b.ColumnAggs = map[string]string{}
		_ = json.Unmarshal(aggs, &b.ColumnAggs)
		b.Exists = true
		out = append(out, b)
	}
	return out, rows.Err()
}

// TitleName is the display name of a title ("" when unnamed).
func (s *Store) TitleNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT key, name FROM titles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
