package api_test

import (
	"context"
	"testing"

	pb "github.com/openpak/leaderboards/proto/openpak/leaderboards/v1"
)

func (e *env) submitG(t *testing.T, r *pb.BoardRef, acct string, score int64, groups []byte, override string) *pb.SubmitResponse {
	t.Helper()
	res, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: acct, Score: score, Groups: groups, AggregationOverride: override})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func gf(i int32, v uint32) *pb.GroupsFilter { return &pb.GroupsFilter{GroupIndex: i, GroupValue: v} }

// L1: groups are stored on the entry and filter Range, Total, ByAccounts and FriendRanks; ranks
// are among the matching entries; entries without the byte never match.
func TestGroupsFilter(t *testing.T) {
	e := setup(t)
	r := ref("wiiu", "mk8", "5")
	e.submitG(t, r, alice, 100, []byte{1, 7}, "")
	e.submitG(t, r, bob, 300, []byte{2, 7}, "")
	e.submitG(t, r, carol, 200, []byte{1}, "") // one byte only
	e.submitG(t, r, dave, 400, nil, "")        // no groups at all
	rg, err := e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, GroupsFilter: gf(0, 1)})
	if err != nil || rg.Total != 2 || len(rg.Entries) != 2 || rg.Entries[0].Account != carol || rg.Entries[0].Rank != 1 ||
		rg.Entries[1].Account != alice || rg.Entries[1].Rank != 2 || string(rg.Entries[1].Groups) != "\x01\x07" {
		t.Fatalf("group 0=1: %v %+v", err, rg)
	}
	rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, GroupsFilter: gf(1, 7)})
	if rg.Total != 2 || rg.Entries[0].Account != bob || rg.Entries[1].Account != alice {
		t.Fatalf("group 1=7: %+v", rg)
	}
	if rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, GroupsFilter: gf(5, 0)}); rg.Total != 0 || len(rg.Entries) != 0 {
		t.Fatalf("index beyond every groups: %+v", rg)
	}
	if rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, GroupsFilter: gf(0, 300)}); rg.Total != 0 {
		t.Fatalf("value above 255 matched: %+v", rg)
	}
	if rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r}); rg.Total != 4 || len(rg.Entries) != 4 {
		t.Fatalf("no filter is the whole board: %+v", rg)
	}
	if rg, _ = e.lb.Range(e.ctx, &pb.RangeRequest{Ref: r, GroupsFilter: gf(-1, 0)}); rg.Total != 4 {
		t.Fatalf("negative index means no filter: %+v", rg)
	}
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r, GroupsFilter: gf(0, 2)}); tot.Total != 1 {
		t.Fatalf("filtered total %d", tot.Total)
	}
	by, _ := e.lb.ByAccounts(e.ctx, &pb.ByAccountsRequest{Ref: r, Accounts: []string{alice}, GroupsFilter: gf(0, 1)})
	if len(by.Entries) != 1 || by.Entries[0].Position != 2 || by.Total != 2 {
		t.Fatalf("by accounts filtered: %+v", by)
	}
	if by, _ = e.lb.ByAccounts(e.ctx, &pb.ByAccountsRequest{Ref: r, Accounts: []string{alice}, GroupsFilter: gf(0, 2)}); len(by.Entries) != 0 {
		t.Fatalf("alice is not in group 2: %+v", by)
	}
	fr, err := e.lb.FriendRanks(e.ctx, &pb.FriendRanksRequest{Ref: r, Account: alice, IncludeSelf: true, GroupsFilter: gf(0, 1)})
	if err != nil || len(fr.Entries) != 2 || fr.Entries[0].Account != carol || fr.Entries[1].Account != alice {
		t.Fatalf("friend ranks filtered: %v %+v", err, fr)
	}
	// Groups are score-bound: a worse score on a best board keeps the stored groups; a better
	// one replaces them.
	e.submitG(t, r, alice, 50, []byte{9, 9}, "")
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r, GroupsFilter: gf(0, 9)}); tot.Total != 0 {
		t.Fatal("a rejected score changed the groups")
	}
	e.submitG(t, r, alice, 500, []byte{9, 9}, "")
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r, GroupsFilter: gf(0, 9)}); tot.Total != 1 {
		t.Fatal("a better score did not replace the groups")
	}
}

// L2: the schema already holds many subs per account (PK account+sub), so no change was needed;
// NEX keeps one score per player per category (sub 0). This pins that both stay true.
func TestSubsStillIndependent(t *testing.T) {
	e := setup(t)
	r := ref("wiiu", "mk8", "6")
	e.submitG(t, r, alice, 10, nil, "")
	if _, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: alice, Sub: 1, Score: 20}); err != nil {
		t.Fatal(err)
	}
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r}); tot.Total != 2 {
		t.Fatalf("two subs of one account %d", tot.Total)
	}
}

// L3: aggregation_override "latest" replaces the entry in one call even with a worse score on a
// best board, and also clears the old game data; other values are refused.
func TestAggregationOverride(t *testing.T) {
	e := setup(t)
	r := ref("wiiu", "mk8", "7")
	e.submitG(t, r, alice, 100, []byte{1}, "")
	if res := e.submitG(t, r, alice, 40, []byte{2}, ""); res.Changed || res.Entry.Score != 100 {
		t.Fatalf("best board kept the better score: %+v", res)
	}
	res := e.submitG(t, r, alice, 40, []byte{2}, "latest")
	if !res.Changed || res.Entry.Score != 40 || res.PreviousScore != 100 || string(res.Entry.Groups) != "\x02" || res.Entry.Rank != 1 {
		t.Fatalf("override latest: %+v", res)
	}
	if res = e.submitG(t, r, alice, 70, nil, "force"); res.Entry.Score != 70 {
		t.Fatalf("force alias: %+v", res)
	}
	// The board itself is unchanged: the next plain submit is still "best".
	if res = e.submitG(t, r, alice, 10, nil, ""); res.Changed || res.Entry.Score != 70 {
		t.Fatalf("board aggregation changed: %+v", res)
	}
	if _, err := e.lb.Submit(e.ctx, &pb.SubmitRequest{Ref: r, Account: alice, Score: 1, AggregationOverride: "sum"}); err == nil {
		t.Fatal("only latest may be forced")
	}
	b, _ := e.lb.BoardInfo(e.ctx, &pb.BoardInfoRequest{Ref: r})
	if b.Board.Policy.Aggregation != "best" {
		t.Fatalf("policy %+v", b.Board.Policy)
	}
}

// Erasure by account covers the groups column: it is on the entry row.
func TestEraseCoversGroups(t *testing.T) {
	e := setup(t)
	r := ref("wiiu", "mk8", "8")
	e.submitG(t, r, alice, 10, []byte{3}, "")
	e.submitG(t, r, bob, 20, []byte{3}, "")
	if n, err := e.svc.EraseAccount(context.Background(), alice); err != nil || n != 1 {
		t.Fatalf("erase %d %v", n, err)
	}
	var left int
	if err := e.svc.Store.DB.QueryRow(context.Background(), `SELECT count(*) FROM entries WHERE account = $1 OR (account = $2 AND false)`, alice, bob).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left for the erased account: %d %v", left, err)
	}
	if tot, _ := e.lb.Total(e.ctx, &pb.TotalRequest{Ref: r, GroupsFilter: gf(0, 3)}); tot.Total != 1 {
		t.Fatalf("the other account's groups survived? %d", tot.Total)
	}
}
