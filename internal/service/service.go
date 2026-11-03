// Package service is the leaderboards' rules on top of the store: title resolution, input
// limits, the friend ranking and the announcements a new best produces. The gRPC and HTTP
// surfaces are thin maps onto it.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	emit "github.com/openpak/rewards-emit"

	"github.com/openpak/leaderboards/internal/store"
)

const (
	MaxComment = 512
	MaxInfo    = 4096
	MaxColumns = 64
	DefaultN   = 25
	MaxN       = 100
)

var (
	ErrBadAccount = errors.New("leaderboards: account ids are UUIDs")
	ErrTooLarge   = errors.New("leaderboards: value too large")
	ErrBadRequest = errors.New("leaderboards: bad request")
	ErrNeedNet    = errors.New("leaderboards: this board is per network: the request must name its network")
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Announcer is the account core as this service uses it.
type Announcer interface {
	Friends(ctx context.Context, account string) ([]string, error)
	Announce(ctx context.Context, account, subject string) error
}

type Service struct {
	Store   *store.Store
	Core    Announcer
	Rewards *emit.Client // nil or disabled: no rewards events
}

// Ref names a board as a caller sees it.
type Ref struct{ Network, Title, Board string }

func (s *Service) resolve(ctx context.Context, r Ref, create bool) (store.Board, error) {
	if r.Board == "" {
		return store.Board{}, ErrBadRequest
	}
	title, err := s.Store.ResolveTitle(ctx, r.Network, r.Title, create)
	if err != nil {
		return store.Board{}, err
	}
	return s.Store.Board(ctx, title, r.Board)
}

// readBoard resolves a board for reading: a missing board is the defaults with nothing on it.
// A per-network board needs the caller's network.
func (s *Service) readBoard(ctx context.Context, r Ref) (store.Board, error) {
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) && r.Network != "" {
		// A title nobody has written to: an empty default board.
		t := r.Title
		return store.Board{Title: t, ID: r.Board, Kind: "score", Order: "desc", Aggregation: store.AggBest, Cross: true,
			RankLimit: store.DefaultRankLimit, DataLimit: store.DefaultDataLimit, SizeLimit: store.DefaultSizeLimit}, errNoTitleEmpty
	}
	if err != nil {
		return b, err
	}
	if !b.Cross && r.Network == "" {
		return b, ErrNeedNet
	}
	return b, nil
}

var errNoTitleEmpty = errors.New("empty")

func validAccount(a string) error {
	if !uuidRE.MatchString(a) {
		return ErrBadAccount
	}
	return nil
}

func clamp(n, def int64) int64 {
	if n <= 0 {
		return def
	}
	return min(n, MaxN)
}

// Submit applies the board's aggregation. A board that does not exist is created from policy.
func (s *Service) Submit(ctx context.Context, r Ref, account string, sub int, score int64, cols map[string]int64, comment string, info []byte, display string, policy store.Policy) (store.Board, store.SubmitResult, int64, error) {
	if err := validAccount(account); err != nil {
		return store.Board{}, store.SubmitResult{}, 0, err
	}
	if r.Network == "" {
		return store.Board{}, store.SubmitResult{}, 0, ErrBadRequest
	}
	if len(comment) > MaxComment || len(info) > MaxInfo || len(cols) > MaxColumns {
		return store.Board{}, store.SubmitResult{}, 0, ErrTooLarge
	}
	title, err := s.Store.ResolveTitle(ctx, r.Network, r.Title, true)
	if err != nil {
		return store.Board{}, store.SubmitResult{}, 0, err
	}
	b, err := s.Store.EnsureBoard(ctx, title, r.Board, policy)
	if err != nil {
		return b, store.SubmitResult{}, 0, err
	}
	res, err := s.Store.Submit(ctx, b, r.Network, account, sub, score, cols, comment, info, display)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil // per-network board and the entry belongs to another network: unranked
	}
	if err != nil {
		return b, res, 0, err
	}
	total, _ := s.Store.Total(ctx, b, r.Network)
	if res.Changed && res.Improved {
		s.announce(ctx, b, account, res)
	}
	return b, res, total, nil
}

// announce is best effort: the entry exists the moment Submit returns.
func (s *Service) announce(ctx context.Context, b store.Board, account string, res store.SubmitResult) {
	if s.Core != nil {
		acx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := s.Core.Announce(acx, account, b.Title+"/"+b.ID); err != nil {
			log.Printf("leaderboards: announce %s/%s for %s: %v", b.Title, b.ID, account, err)
		}
		cancel()
	}
	if s.Rewards.Enabled() {
		s.Rewards.Event(emit.Event{
			ID:      fmt.Sprintf("lb:%s:%s:%s:%d", b.Title, b.ID, account, res.Entry.Updated.UnixMilli()),
			Metric:  "leaderboard_new_best." + b.Title,
			Account: account,
		})
	}
}

func (s *Service) Range(ctx context.Context, r Ref, start, limit int64) (store.Board, []store.Entry, int64, error) {
	b, err := s.readBoard(ctx, r)
	if errors.Is(err, errNoTitleEmpty) {
		return b, nil, 0, nil
	}
	if err != nil {
		return b, nil, 0, err
	}
	n := clamp(limit, DefaultN)
	es, err := s.Store.Range(ctx, b, r.Network, max(start, 1), n)
	if err != nil {
		return b, nil, 0, err
	}
	total, err := s.Store.Total(ctx, b, r.Network)
	return b, es, total, err
}

func (s *Service) ByAccounts(ctx context.Context, r Ref, accounts []string) (store.Board, []store.Entry, int64, error) {
	for _, a := range accounts {
		if err := validAccount(a); err != nil {
			return store.Board{}, nil, 0, err
		}
	}
	b, err := s.readBoard(ctx, r)
	if errors.Is(err, errNoTitleEmpty) {
		return b, nil, 0, nil
	}
	if err != nil {
		return b, nil, 0, err
	}
	if len(accounts) == 0 {
		return b, nil, 0, nil
	}
	es, err := s.Store.Accounts(ctx, b, r.Network, accounts)
	if err != nil {
		return b, nil, 0, err
	}
	total, err := s.Store.Total(ctx, b, r.Network)
	return b, es, total, err
}

// FriendRanks is the best entry of each friend (and optionally the caller) in rank order, ranks
// global. The friend list is the account core's, so the rule is the same for every network.
func (s *Service) FriendRanks(ctx context.Context, r Ref, account string, includeSelf bool, offset, limit int64) (store.Board, []store.Entry, int64, int64, error) {
	if err := validAccount(account); err != nil {
		return store.Board{}, nil, 0, 0, err
	}
	friends, err := s.Core.Friends(ctx, account)
	if err != nil {
		return store.Board{}, nil, 0, 0, fmt.Errorf("leaderboards: friends: %w", err)
	}
	if includeSelf {
		friends = append(friends, account)
	}
	b, es, total, err := s.ByAccounts(ctx, r, friends)
	if err != nil {
		return b, nil, 0, 0, err
	}
	seen := map[string]bool{}
	var best []store.Entry
	for _, e := range es { // best first, so the first row of an account is its best
		if !seen[e.Account] {
			seen[e.Account] = true
			best = append(best, e)
		}
	}
	hits := int64(len(best))
	lo := min(max(offset, 0), hits)
	hi := min(lo+clamp(limit, MaxN), hits)
	return b, best[lo:hi], total, hits, nil
}

func (s *Service) BoardInfo(ctx context.Context, r Ref) (store.Board, error) {
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) {
		return store.Board{Title: r.Title, ID: r.Board, Kind: "score", Order: "desc", Aggregation: store.AggBest, Cross: true,
			RankLimit: store.DefaultRankLimit, DataLimit: store.DefaultDataLimit, SizeLimit: store.DefaultSizeLimit}, nil
	}
	return b, err
}

func (s *Service) Total(ctx context.Context, r Ref) (int64, error) {
	b, err := s.readBoard(ctx, r)
	if errors.Is(err, errNoTitleEmpty) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return s.Store.Total(ctx, b, r.Network)
}

// SetData stores game data on the entry whose score is exactly expect.
func (s *Service) SetData(ctx context.Context, r Ref, account string, sub int, expect int64, data []byte) (bool, error) {
	if err := validAccount(account); err != nil {
		return false, err
	}
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(data) > b.SizeLimit {
		return false, ErrTooLarge
	}
	return s.Store.SetData(ctx, b, account, sub, expect, data)
}

func (s *Service) GetData(ctx context.Context, r Ref, account string, sub int) ([]byte, error) {
	if err := validAccount(account); err != nil {
		return nil, err
	}
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Store.Data(ctx, b, account, sub)
}

func (s *Service) Delete(ctx context.Context, r Ref, account string, oneSub bool, sub int) (int64, error) {
	if err := validAccount(account); err != nil {
		return 0, err
	}
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return s.Store.Delete(ctx, b, account, oneSub, sub)
}

func (s *Service) EraseAccount(ctx context.Context, account string) (int64, error) {
	if err := validAccount(account); err != nil {
		return 0, err
	}
	return s.Store.EraseAccount(ctx, account)
}

func (s *Service) UpsertBoard(ctx context.Context, r Ref, titleName string, p store.Policy) (store.Board, error) {
	if r.Board == "" || r.Title == "" {
		return store.Board{}, ErrBadRequest
	}
	title, err := s.Store.ResolveTitle(ctx, r.Network, r.Title, true)
	if err != nil {
		return store.Board{}, err
	}
	if titleName != "" {
		if err := s.Store.MapTitle(ctx, title, titleName, "", ""); err != nil {
			return store.Board{}, err
		}
	}
	return s.Store.UpsertBoard(ctx, title, r.Board, p)
}

func (s *Service) ResetBoard(ctx context.Context, r Ref) (int64, error) {
	b, err := s.resolve(ctx, r, false)
	if errors.Is(err, store.ErrNoTitle) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return s.Store.Reset(ctx, b)
}
