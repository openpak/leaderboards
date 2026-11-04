// Package api is the leaderboards' HTTP surface.
//
//	GET  /healthz
//	DELETE /internal/accounts/{account}/entries   account erasure: every entry of the account (X-Internal-Key) -> {"deleted":n}
//
// And the website's read-only rankings contract (docs/rankings-contract.md in the website
// repo), unauthenticated and for the container network only, in three scopes:
//
//	/internal/rankings/...                      every title's cross-network boards, board id "<title>.<board>"
//	/t/{title}/internal/rankings/...            one title ({title} is the neutral key), board id "<board>"
//	/n/{network}/t/{title}/internal/rankings/...  the same seen from one network: boards that opt out of
//	                                            cross-network show only that network's entries
//
// The website's rankingServers entry for a title is therefore just a base URL with a path:
// http://openpak-leaderboards:20210/t/terraria.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/openpak/leaderboards/internal/rankingsapi"
	"github.com/openpak/leaderboards/internal/service"
	"github.com/openpak/leaderboards/internal/store"
)

type Server struct {
	Svc *service.Service
	Key string
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Svc.Store.DB.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("DELETE /internal/accounts/{account}/entries", s.auth(s.erase))
	mux.Handle("/internal/rankings/", s.scope("", ""))
	mux.HandleFunc("/t/{title}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		s.scope("", r.PathValue("title")).ServeHTTP(w, r)
	})
	mux.HandleFunc("/n/{network}/t/{title}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		// The title segment is a native id on that network (an NPWR id, an Xbox title id, a Switch
		// application id) or already the neutral key: resolve it, and keep the raw value when it
		// is not mapped.
		network, title := r.PathValue("network"), r.PathValue("title")
		if k, err := s.Svc.Store.ResolveTitle(r.Context(), network, title, false); err == nil {
			title = k
		}
		s.scope(network, title).ServeHTTP(w, r)
	})
}

// scope serves the rankings contract for (network, title); "" means all. The contract paths
// are matched after the scope prefix is stripped.
func (s *Server) scope(network, title string) http.Handler {
	h := rankingsapi.Handler("", &source{svc: s.Svc, network: network, title: title})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i := strings.Index(r.URL.Path, "/internal/rankings/"); i > 0 {
			r2 := r.Clone(r.Context())
			r2.URL.Path = r.URL.Path[i:]
			r = r2
		}
		// ?all=1 on the board list also lists boards nobody has scored on yet (the website's "no scores yet" view).
		if r.URL.Query().Get("all") == "1" {
			r = r.WithContext(context.WithValue(r.Context(), allBoardsKey{}, true))
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(s.Key)) != 1 {
			jsonOut(w, http.StatusUnauthorized, map[string]any{"error": "internal key"})
			return
		}
		h(w, r)
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// erase is the account-erasure finaliser's call once the deletion grace period is over; a repeat is a 200.
func (s *Server) erase(w http.ResponseWriter, r *http.Request) {
	n, err := s.Svc.EraseAccount(r.Context(), r.PathValue("account"))
	switch {
	case errors.Is(err, service.ErrBadAccount):
		jsonOut(w, http.StatusBadRequest, map[string]any{"error": "account ids are UUIDs"})
	case err != nil:
		jsonOut(w, http.StatusBadGateway, map[string]any{"error": "leaderboards service error"})
	default:
		jsonOut(w, 200, map[string]any{"deleted": n})
	}
}

// source adapts the store to rankingsapi.Source.
type source struct {
	svc     *service.Service
	network string // "" = unscoped: only cross-network boards
	title   string // neutral key; "" = every title
}

func (s *source) visible(b store.Board) bool { return b.Cross || s.network != "" }

func neutral(b store.Board) string {
	if b.Name != "" {
		return b.Name
	}
	return "Board " + b.ID
}

type allBoardsKey struct{}

func (s *source) Boards(ctx context.Context) ([]rankingsapi.Board, error) {
	all, _ := ctx.Value(allBoardsKey{}).(bool)
	bs, err := s.svc.Store.Boards(ctx, s.title, !all)
	if err != nil {
		return nil, err
	}
	var names map[string]string
	if s.title == "" {
		if names, err = s.svc.Store.TitleNames(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]rankingsapi.Board, 0, len(bs))
	for _, b := range bs {
		if !s.visible(b) {
			continue
		}
		id, name := b.ID, neutral(b)
		if s.title == "" {
			id = b.Title + "." + b.ID
			t := names[b.Title]
			if t == "" {
				t = b.Title
			}
			name = t + ": " + name
		}
		kind := b.Kind
		if kind != rankingsapi.KindTime && kind != rankingsapi.KindRating {
			kind = rankingsapi.KindScore
		}
		out = append(out, rankingsapi.Board{ID: id, Name: name, Kind: kind, Order: b.Order, Unit: b.Unit, Season: b.Season})
	}
	return out, nil
}

func (s *source) find(ctx context.Context, id string) (store.Board, error) {
	title, board := s.title, id
	if title == "" {
		var ok bool
		title, board, ok = strings.Cut(id, ".")
		if !ok {
			return store.Board{}, rankingsapi.ErrNoBoard
		}
	}
	b, err := s.svc.Store.Board(ctx, title, board)
	if err != nil {
		return b, err
	}
	if !b.Exists || !s.visible(b) {
		return b, rankingsapi.ErrNoBoard
	}
	return b, nil
}

func rows(b store.Board, es []store.Entry) []rankingsapi.Row {
	out := make([]rankingsapi.Row, 0, len(es))
	for _, e := range es {
		out = append(out, rankingsapi.Row{Rank: int(e.Rank), AccountID: e.Account, Score: e.Score,
			Display: rankingsapi.Format(b.Kind, e.Score), UpdatedAt: e.Updated.UTC()})
	}
	return out
}

func (s *source) Rows(ctx context.Context, id string, q rankingsapi.Query) (rankingsapi.Page, error) {
	b, err := s.find(ctx, id)
	if err != nil {
		return rankingsapi.Page{}, err
	}
	total, err := s.svc.Store.Total(ctx, b, s.network)
	if err != nil {
		return rankingsapi.Page{}, err
	}
	if total == 0 {
		return rankingsapi.Page{}, nil
	}
	es, err := s.svc.Store.Range(ctx, b, s.network, int64(q.Offset)+1, int64(q.Limit))
	if err != nil {
		return rankingsapi.Page{}, err
	}
	p := rankingsapi.Page{Total: int(total), Rows: rows(b, es)}
	if q.Around != "" {
		mine, err := s.svc.Store.Accounts(ctx, b, s.network, []string{q.Around})
		if err == nil && len(mine) > 0 {
			near, err := s.svc.Store.Range(ctx, b, s.network, max(mine[0].Position-rankingsapi.AroundSpan, 1), 2*rankingsapi.AroundSpan+1)
			if err != nil {
				return rankingsapi.Page{}, err
			}
			p.Around = rows(b, near)
		}
	}
	return p, nil
}
