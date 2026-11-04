// The leaderboards flow against a real Postgres (LEADERBOARDS_TEST_DATABASE_URL) and a fake
// account core over bufconn: aggregation, ranks, per-column merge, game data, cross-network
// visibility, friend ranks, title keys and the website rankings contract. Skipped without a
// database, like every integration surface here.
package api_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"github.com/openpak/leaderboards/internal/api"
	"github.com/openpak/leaderboards/internal/core"
	"github.com/openpak/leaderboards/internal/grpcapi"
	"github.com/openpak/leaderboards/internal/service"
	"github.com/openpak/leaderboards/internal/store"
	pb "github.com/openpak/leaderboards/proto/openpak/leaderboards/v1"
)

const (
	alice = "11111111-1111-1111-1111-111111111111"
	bob   = "22222222-2222-2222-2222-222222222222"
	carol = "33333333-3333-3333-3333-333333333333"
	dave  = "44444444-4444-4444-4444-444444444444"
	key   = "test-key"
)

type fakeCore struct {
	accountv1.UnimplementedSocialServer
	accountv1.UnimplementedEventsServer
	friends   map[string][]string
	announced []string
}

func (f *fakeCore) ListFriends(_ context.Context, req *accountv1.ListFriendsRequest) (*accountv1.ListFriendsResponse, error) {
	out := &accountv1.ListFriendsResponse{}
	for _, id := range f.friends[req.GetAccountId()] {
		out.Friends = append(out.Friends, &accountv1.Friend{AccountId: id})
	}
	return out, nil
}

func (f *fakeCore) EmitEvent(_ context.Context, req *accountv1.EmitEventRequest) (*accountv1.EmitEventResponse, error) {
	f.announced = append(f.announced, req.GetType()+"|"+req.GetAccountId()+"|"+req.GetSubjectId())
	return &accountv1.EmitEventResponse{}, nil
}

type env struct {
	lb   pb.LeaderboardsClient
	fake *fakeCore
	http *httptest.Server
	svc  *service.Service
	ctx  context.Context
}

func setup(t *testing.T) *env {
	t.Helper()
	dbURL := os.Getenv("LEADERBOARDS_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEADERBOARDS_TEST_DATABASE_URL not set")
	}
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.DB.Exec(context.Background(), `TRUNCATE entries, boards, title_keys, titles`); err != nil {
		t.Fatal(err)
	}
	// The seed rows the migration inserts, restored after the truncate.
	if err := st.MapTitle(context.Background(), "terraria", "Terraria", "np", "NPWR04270_00"); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCore{friends: map[string][]string{alice: {bob, carol}}}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	accountv1.RegisterSocialServer(gs, fake)
	accountv1.RegisterEventsServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	dial := func(l *bufconn.Listener) *grpc.ClientConn {
		c, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	cc := dial(lis)
	svc := &service.Service{Store: st, Core: core.New(accountv1.NewSocialClient(cc), accountv1.NewEventsClient(cc), key)}

	lis2 := bufconn.Listen(1 << 20)
	gs2 := grpc.NewServer()
	pb.RegisterLeaderboardsServer(gs2, grpcapi.New(svc, key))
	go func() { _ = gs2.Serve(lis2) }()
	t.Cleanup(gs2.Stop)

	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: key}).Register(mux)
	h := httptest.NewServer(mux)
	t.Cleanup(h.Close)
	return &env{lb: pb.NewLeaderboardsClient(dial(lis2)), fake: fake, http: h, svc: svc,
		ctx: metadata.AppendToOutgoingContext(context.Background(), "x-internal-key", key)}
}

func ref(network, title, board string) *pb.BoardRef {
	return &pb.BoardRef{Network: network, Title: title, Board: board}
}

func (e *env) submit(t *testing.T, r *pb.BoardRef, acct string, score int64) *pb.SubmitResponse {
	t.Helper()
	res, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: acct, Score: score, Comment: "c", DisplayName: "n"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAuthRequired(t *testing.T) {
	e := setup(t)
	if _, err := e.lb.Total(context.Background(), &pb.TotalRequest{Ref: ref("np", "NPWR04270_00", "0")}); err == nil {
		t.Fatal("call without the internal key succeeded")
	}
}

func TestBestAggregationAndRanks(t *testing.T) {
	e := setup(t)
	r := ref("np", "NPWR04270_00", "0") // Terraria via the seeded title key
	a := e.submit(t, r, alice, 100)
	if !a.Changed || a.Entry.Rank != 1 || a.Total != 1 || a.HadPrevious {
		t.Fatalf("first submit: %+v", a)
	}
	if res := e.submit(t, r, alice, 50); res.Changed || res.Entry.Score != 100 {
		t.Fatalf("worse score replaced the best: %+v", res)
	}
	if res := e.submit(t, r, alice, 100); res.Changed {
		t.Fatalf("an equal score counts as a new best: %+v", res)
	}
	b := e.submit(t, r, bob, 200)
	if b.Entry.Rank != 1 || b.Total != 2 {
		t.Fatalf("bob should lead: %+v", b)
	}
	c := e.submit(t, r, carol, 100) // ties alice: competition rank 2, later position
	if c.Entry.Rank != 2 || c.Entry.Position != 3 {
		t.Fatalf("tie: rank %d position %d", c.Entry.Rank, c.Entry.Position)
	}
	if up := e.submit(t, r, alice, 300); !up.Changed || up.PreviousScore != 100 || up.Entry.Rank != 1 {
		t.Fatalf("improvement: %+v", up)
	}
	rg, err := e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, Start: 2, Limit: 2})
	if err != nil || len(rg.Entries) != 2 || rg.Entries[0].Account != bob || rg.Total != 3 {
		t.Fatalf("range: %v %+v", err, rg)
	}
	// The same title is reachable by its neutral key.
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: ref("", "terraria", "0")}); tot.Total != 3 {
		t.Fatalf("neutral key total %d", tot.Total)
	}
	// Improvements were announced on the core's stream; non-improvements were not.
	want := 4 // alice 100, bob 200, carol 100, alice 300
	if len(e.fake.announced) != want || e.fake.announced[0] != "leaderboard.record|"+alice+"|terraria/0" {
		t.Fatalf("announced %v", e.fake.announced)
	}
}

func TestAscendingAndSum(t *testing.T) {
	e := setup(t)
	asc := ref("wiiu", "mk8", "5")
	if _, err := e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: asc, Policy: &pb.Policy{Order: "asc", Kind: "time"}}); err != nil {
		t.Fatal(err)
	}
	e.submit(t, asc, alice, 90000)
	if r := e.submit(t, asc, alice, 95000); r.Changed {
		t.Fatal("slower time replaced the best on an ascending board")
	}
	if r := e.submit(t, asc, bob, 80000); r.Entry.Rank != 1 {
		t.Fatalf("faster time not first: %+v", r.Entry)
	}
	sum := ref("x360", "halo3", "kills")
	e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: sum, Policy: &pb.Policy{Aggregation: "sum"}})
	e.submit(t, sum, alice, 10)
	if r := e.submit(t, sum, alice, 5); r.Entry.Score != 15 {
		t.Fatalf("sum: %d", r.Entry.Score)
	}
	lat := ref("x360", "halo3", "last")
	e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: lat, Policy: &pb.Policy{Aggregation: "force"}})
	e.submit(t, lat, alice, 10)
	if r := e.submit(t, lat, alice, 3); !r.Changed || r.Entry.Score != 3 {
		t.Fatalf("force/latest: %+v", r.Entry)
	}
}

func TestPerColumnAggregation(t *testing.T) {
	e := setup(t)
	r := ref("x360", "4d530064", "view1")
	if _, err := e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: r, Policy: &pb.Policy{
		ScoreColumn: "1", ColumnAggregations: map[string]string{"1": "sum", "2": "max", "3": "min", "4": "latest"}}}); err != nil {
		t.Fatal(err)
	}
	post := func(cols map[string]int64) *pb.SubmitResponse {
		res, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: alice, Columns: cols})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	post(map[string]int64{"1": 10, "2": 5, "3": 9, "4": 1})
	res := post(map[string]int64{"1": 7, "2": 3, "3": 4, "4": 2})
	got := res.Entry.Columns
	if got["1"] != 17 || got["2"] != 5 || got["3"] != 4 || got["4"] != 2 || res.Entry.Score != 17 {
		t.Fatalf("columns %v score %d", got, res.Entry.Score)
	}
}

func TestGameData(t *testing.T) {
	e := setup(t)
	r := ref("np", "NPWR04270_00", "1")
	e.submit(t, r, alice, 100)
	if res, _ := e.lb.SetData(e.ctx, &pb.SetDataRequest{Ref: r, Account: alice, ExpectScore: 99, Data: []byte("x")}); res.Stored {
		t.Fatal("data stored against a score the player does not hold")
	}
	if res, _ := e.lb.SetData(e.ctx, &pb.SetDataRequest{Ref: r, Account: alice, ExpectScore: 100, Data: []byte("ghost")}); !res.Stored {
		t.Fatal("data not stored")
	}
	g, _ := e.lb.GetData(e.ctx, &pb.GetDataRequest{Ref: r, Account: alice})
	if !g.Found || string(g.Data) != "ghost" {
		t.Fatalf("get data %+v", g)
	}
	rg, _ := e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r})
	if !rg.Entries[0].HasData {
		t.Fatal("entry should report data")
	}
	e.submit(t, r, alice, 200) // a new best drops the old game data
	if g, _ := e.lb.GetData(e.ctx, &pb.GetDataRequest{Ref: r, Account: alice}); g.Found {
		t.Fatal("game data survived a new best")
	}
	e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: r, Policy: &pb.Policy{SizeLimit: 4}})
	if _, err := e.lb.SetData(e.ctx, &pb.SetDataRequest{Ref: r, Account: alice, ExpectScore: 200, Data: []byte("12345")}); err == nil {
		t.Fatal("oversized data accepted")
	}
}

func TestCrossNetwork(t *testing.T) {
	e := setup(t)
	open := ref("np", "NPWR04270_00", "2")
	e.submit(t, open, alice, 10)
	// Same board seen from another network (neutral key): cross-network is the default.
	if _, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: ref("x360", "terraria-x", "2"), Account: bob, Score: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Store.MapTitle(e.ctx, "terraria", "", "x360", "5841081c"); err != nil {
		t.Fatal(err)
	}
	e.submit(t, ref("x360", "5841081c", "2"), bob, 20)
	rg, _ := e.lb.Range(e.ctx, &pb.RangeRequest{Ref: ref("np", "NPWR04270_00", "2")})
	if len(rg.Entries) != 2 || rg.Entries[0].Account != bob || rg.Entries[0].Network != "x360" || rg.Entries[1].Network != "np" {
		t.Fatalf("cross-network board: %+v", rg.Entries)
	}
	// Opt out: each network ranks only itself.
	f := false
	_ = f
	if _, err := e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: ref("np", "NPWR04270_00", "2"), Policy: &pb.Policy{CrossNetwork: pb.Tristate_TRISTATE_FALSE}}); err != nil {
		t.Fatal(err)
	}
	rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: ref("np", "NPWR04270_00", "2")})
	if len(rg.Entries) != 1 || rg.Entries[0].Account != alice {
		t.Fatalf("opt-out board leaked other networks: %+v", rg.Entries)
	}
	if _, err := e.lb.Range(e.ctx, &pb.RangeRequest{Ref: ref("", "terraria", "2")}); err == nil {
		t.Fatal("an opt-out board answered a caller with no network")
	}
}

func TestFriendRanks(t *testing.T) {
	e := setup(t)
	r := ref("np", "NPWR04270_00", "3")
	e.submit(t, r, alice, 10)
	e.submit(t, r, bob, 30)
	e.submit(t, r, dave, 99) // not alice's friend
	// A second pcId of bob's must not show him twice.
	if _, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: bob, Sub: 1, Score: 20}); err != nil {
		t.Fatal(err)
	}
	res, err := e.lb.FriendRanks(e.ctx, &pb.FriendRanksRequest{Ref: r, Account: alice, IncludeSelf: true})
	if err != nil || res.Hits != 2 || len(res.Entries) != 2 {
		t.Fatalf("friend ranks: %v %+v", err, res)
	}
	if res.Entries[0].Account != bob || res.Entries[0].Rank != 2 || res.Entries[1].Account != alice || res.Entries[1].Rank != 4 {
		t.Fatalf("friend ranks are global: %+v", res.Entries)
	}
}

func TestRankingsContractAndErase(t *testing.T) {
	e := setup(t)
	r := ref("np", "NPWR04270_00", "0")
	e.lb.UpsertBoard(e.ctx, &pb.UpsertBoardRequest{Ref: r, Policy: &pb.Policy{Name: "Distance"}})
	for i, a := range []string{alice, bob, carol} {
		e.submit(t, r, a, int64(100*(i+1)))
	}
	get := func(path string, out any) int {
		resp, err := http.Get(e.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	var boards struct {
		Boards []struct{ ID, Name, Order string }
	}
	if get("/t/terraria/internal/rankings/boards", &boards) != 200 || len(boards.Boards) != 1 || boards.Boards[0].ID != "0" || boards.Boards[0].Name != "Distance" {
		t.Fatalf("per-title boards: %+v", boards)
	}
	if get("/internal/rankings/boards", &boards) != 200 || boards.Boards[0].ID != "terraria.0" || boards.Boards[0].Name != "Terraria: Distance" {
		t.Fatalf("all boards: %+v", boards)
	}
	var page struct {
		Total int
		Rows  []struct {
			Rank      int
			AccountID string `json:"account_id"`
			Score     int64
			Display   string
		}
		Around []struct{ Rank int }
	}
	if get("/t/terraria/internal/rankings/boards/0?limit=2&around="+bob, &page) != 200 || page.Total != 3 || len(page.Rows) != 2 ||
		page.Rows[0].AccountID != carol || page.Rows[0].Rank != 1 || page.Rows[0].Display != "300" || len(page.Around) != 3 {
		t.Fatalf("page: %+v", page)
	}
	if get("/internal/rankings/boards/terraria.0?limit=1", &page) != 200 || page.Rows[0].Score != 300 {
		t.Fatalf("all-titles board: %+v", page)
	}
	if get("/t/terraria/internal/rankings/boards/9", nil) != 404 {
		t.Fatal("unknown board should be 404")
	}
	if get("/n/np/t/terraria/internal/rankings/boards", &boards) != 200 || len(boards.Boards) != 1 {
		t.Fatal("network-scoped route")
	}
	// A native id resolves through title_keys; an unmapped one stays raw and is empty.
	if _, err := e.lb.MapTitle(e.ctx, &pb.MapTitleRequest{Title: "terraria", Name: "Terraria", Network: "np", NativeId: "NPWR04270_00"}); err != nil {
		t.Fatal(err)
	}
	if get("/n/np/t/NPWR04270_00/internal/rankings/boards", &boards) != 200 || len(boards.Boards) != 1 {
		t.Fatalf("native id route: %+v", boards)
	}
	if get("/n/np/t/NPWR99999_00/internal/rankings/boards", &boards) != 200 || len(boards.Boards) != 0 {
		t.Fatalf("unmapped native id: %+v", boards)
	}

	// Erasure needs the key, then removes the account everywhere; a repeat is a 200.
	del := func(k string) int {
		req, _ := http.NewRequest("DELETE", e.http.URL+"/internal/accounts/"+alice+"/entries", nil)
		if k != "" {
			req.Header.Set("X-Internal-Key", k)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if del("") != 401 || del(key) != 200 || del(key) != 200 {
		t.Fatal("erasure statuses")
	}
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r}); tot.Total != 2 {
		t.Fatalf("total after erasure %d", tot.Total)
	}
}

func TestDeleteAndReset(t *testing.T) {
	e := setup(t)
	r := ref("np", "NPWR04270_00", "4")
	e.submit(t, r, alice, 1)
	e.submit(t, r, bob, 2)
	d, _ := e.lb.Delete(e.ctx, &pb.DeleteRequest{Ref: r, Account: alice})
	if d.Deleted != 1 {
		t.Fatalf("deleted %d", d.Deleted)
	}
	rs, _ := e.lb.ResetBoard(e.ctx, &pb.ResetBoardRequest{Ref: r})
	if rs.Deleted != 1 {
		t.Fatalf("reset %d", rs.Deleted)
	}
}
