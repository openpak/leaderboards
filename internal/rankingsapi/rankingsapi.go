// Package rankingsapi is the OpenPak rankings contract (docs/rankings-contract.md): the
// read-only internal HTTP JSON API every game server implements, and the client the website
// reads it with.
//
// Game servers copy this one file into their repo (standard library only, same precedent as
// the other shared code) and implement Source over the rankings they already store:
//
//	rankingsapi.Serve(os.Getenv("RANKINGS_ADDR"), "wiiu", mySource)
//
// Serve does nothing when the address is empty, so the endpoint is off unless the deployment
// asks for it, and it listens on the container network only (no Traefik route, no published
// port). Servers never send display names or avatars: the website resolves those through the
// account core from AccountID or PID.
package rankingsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Board kinds and sort orders.
const (
	KindTime   = "time"   // lower is better, Score is milliseconds
	KindScore  = "score"  // higher is better
	KindRating = "rating" // higher is better, a skill or rank rating

	OrderAsc  = "asc"  // rank 1 has the lowest score
	OrderDesc = "desc" // rank 1 has the highest score
)

// Paging limits.
const (
	DefaultLimit = 25
	MaxLimit     = 100
	// AroundSpan is how many rows each side of the player the "around" slice holds.
	AroundSpan = 5
)

// Board is one ranking: a course, a mode, a season.
type Board struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Order  string `json:"order"`
	Unit   string `json:"unit,omitempty"`
	Season string `json:"season,omitempty"`
}

// Row is one ranked entry. A server sets PID (the player's id in the console network, a
// decimal string) or AccountID (the OpenPak account id) -- whichever its table stores.
type Row struct {
	Rank      int       `json:"rank"`
	AccountID string    `json:"account_id,omitempty"`
	PID       string    `json:"pid,omitempty"`
	Score     int64     `json:"score"`
	Display   string    `json:"display"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Page is the answer for one board. Rows is the requested window; Around is the slice around
// the player named by the around query parameter, empty when none was asked for or the player
// is not on the board.
type Page struct {
	Board  string `json:"board"`
	Total  int    `json:"total"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Rows   []Row  `json:"rows"`
	Around []Row  `json:"around,omitempty"`
}

// BoardList is the answer for GET /internal/rankings/boards. Network is the account-core link
// namespace PID belongs to ("wiiu", "switch", "wfc", "np", "x360", "xbox"); rows that carry
// AccountID do not need it.
type BoardList struct {
	Network string  `json:"network,omitempty"`
	Boards  []Board `json:"boards"`
}

// Query is what a server is asked for one board.
type Query struct {
	Limit, Offset int
	// Around is the account id or PID (the same kind the rows carry) to find, "" for none.
	Around string
}

// ErrNoBoard is returned by Source.Rows for an id that is not a board.
var ErrNoBoard = errors.New("rankings: no such board")

// Source is what a game server implements over its stored rankings. Rows returns the window
// [Offset, Offset+Limit) in rank order with Rank filled in, the board's Total, and, when
// Query.Around is set and that player is ranked, the rows AroundSpan either side of them
// in Page.Around. Handler fills Board, Offset, Limit and the display strings it can.
type Source interface {
	Boards(ctx context.Context) ([]Board, error)
	Rows(ctx context.Context, board string, q Query) (Page, error)
}

// Handler serves the contract over src. network is BoardList.Network.
func Handler(network string, src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/rankings/boards", func(w http.ResponseWriter, r *http.Request) {
		boards, err := src.Boards(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if boards == nil {
			boards = []Board{}
		}
		write(w, http.StatusOK, BoardList{Network: network, Boards: boards})
	})
	mux.HandleFunc("GET /internal/rankings/boards/{id}", func(w http.ResponseWriter, r *http.Request) {
		q := Query{Limit: DefaultLimit, Around: r.URL.Query().Get("around")}
		for name, dst := range map[string]*int{"limit": &q.Limit, "offset": &q.Offset} {
			if v := r.URL.Query().Get(name); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					write(w, http.StatusBadRequest, map[string]string{"error": name + " must be a non-negative integer"})
					return
				}
				*dst = n
			}
		}
		q.Limit = min(max(q.Limit, 1), MaxLimit)
		id := r.PathValue("id")
		p, err := src.Rows(r.Context(), id, q)
		if err != nil {
			fail(w, err)
			return
		}
		p.Board, p.Offset, p.Limit = id, q.Offset, q.Limit
		if p.Rows == nil {
			p.Rows = []Row{}
		}
		write(w, http.StatusOK, p)
	})
	return mux
}

// Serve starts the contract on addr in the background; an empty addr leaves it off.
func Serve(addr, network string, src Source) {
	if addr == "" {
		return
	}
	go func() {
		srv := &http.Server{Addr: addr, Handler: Handler(network, src), ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("rankings: %v", err)
		}
	}()
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNoBoard) {
		write(w, http.StatusNotFound, map[string]string{"error": "no such board"})
		return
	}
	log.Printf("rankings: %v", err)
	write(w, http.StatusServiceUnavailable, map[string]string{"error": "store unavailable"})
}

// NeutralName is the board name for an id the server has no known name for: "Board 3", never
// a raw hex id.
func NeutralName(n uint64) string { return "Board " + strconv.FormatUint(n, 10) }

// Number is n with thousands separators ("12,345").
func Number(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Time is a time in milliseconds as m:ss.mmm.
func Time(ms int64) string { return fmt.Sprintf("%d:%02d.%03d", ms/60000, ms/1000%60, ms%1000) }

// Format shows score the way the board's kind reads it: Time for time boards, Number otherwise.
func Format(kind string, score int64) string {
	if kind == KindTime {
		return Time(score)
	}
	return Number(score)
}

// Window cuts a full, rank-ordered row list into a Page: the Offset/Limit window, Total, and
// the Around slice for the row whose AccountID or PID equals q.Around. It numbers ranks
// (1-based, ties by position) and fills Display from kind where a row has none. For a server
// that holds a board in memory or can fetch it whole; a server with a big table pages in SQL
// and builds the Page itself.
func Window(kind string, all []Row, q Query) Page {
	for i := range all {
		all[i].Rank = i + 1
		if all[i].Display == "" {
			all[i].Display = Format(kind, all[i].Score)
		}
	}
	p := Page{Total: len(all), Rows: []Row{}}
	lo := min(q.Offset, len(all))
	p.Rows = append(p.Rows, all[lo:min(lo+q.Limit, len(all))]...)
	if q.Around != "" {
		for i, r := range all {
			if r.AccountID == q.Around || r.PID == q.Around {
				p.Around = append([]Row(nil), all[max(i-AroundSpan, 0):min(i+AroundSpan+1, len(all))]...)
				break
			}
		}
	}
	return p
}

// Client reads another service's contract endpoint; the website holds one per title.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns nil for an empty base URL.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// get decodes a 200; a 404 is ErrNoBoard, anything else an error.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	case http.StatusNotFound:
		return ErrNoBoard
	}
	return fmt.Errorf("rankings: %s", resp.Status)
}

// Boards fetches GET /internal/rankings/boards.
func (c *Client) Boards(ctx context.Context) (BoardList, error) {
	var l BoardList
	err := c.get(ctx, "/internal/rankings/boards", nil, &l)
	return l, err
}

// Rows fetches GET /internal/rankings/boards/{id}.
func (c *Client) Rows(ctx context.Context, board string, q Query) (Page, error) {
	v := url.Values{"limit": {strconv.Itoa(q.Limit)}, "offset": {strconv.Itoa(q.Offset)}}
	if q.Around != "" {
		v.Set("around", q.Around)
	}
	var p Page
	err := c.get(ctx, "/internal/rankings/boards/"+url.PathEscape(board), v, &p)
	return p, err
}
