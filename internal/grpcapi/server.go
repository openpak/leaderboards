// Package grpcapi is the leaderboards' gRPC surface for the console-side adapters (np-score,
// x360-xstats, nex, wfc): the same rules as the HTTP surface, one wire.
package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jackc/pgx/v5"

	"github.com/openpak/leaderboards/internal/service"
	"github.com/openpak/leaderboards/internal/store"
	pb "github.com/openpak/leaderboards/proto/openpak/leaderboards/v1"
)

type Server struct {
	pb.UnimplementedLeaderboardsServer
	svc *service.Service
	key string
}

func New(svc *service.Service, internalKey string) *Server {
	return &Server{svc: svc, key: internalKey}
}

func (s *Server) authorize(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || s.key == "" {
		return status.Error(codes.Unauthenticated, "missing internal credentials")
	}
	if vals := md.Get("x-internal-key"); len(vals) == 0 || vals[0] != s.key {
		return status.Error(codes.Unauthenticated, "missing internal credentials")
	}
	return nil
}

func ref(r *pb.BoardRef) service.Ref {
	return service.Ref{Network: r.GetNetwork(), Title: r.GetTitle(), Board: r.GetBoard()}
}

func tri(t pb.Tristate) *bool {
	switch t {
	case pb.Tristate_TRISTATE_TRUE:
		v := true
		return &v
	case pb.Tristate_TRISTATE_FALSE:
		v := false
		return &v
	}
	return nil
}

func policy(p *pb.Policy) store.Policy {
	if p == nil {
		return store.Policy{}
	}
	return store.Policy{Name: p.GetName(), Kind: p.GetKind(), Order: p.GetOrder(), Aggregation: p.GetAggregation(),
		ColumnAggs: p.GetColumnAggregations(), ScoreColumn: p.GetScoreColumn(), Unit: p.GetUnit(), Season: p.GetSeason(),
		RankLimit: int(p.GetRankLimit()), DataLimit: int(p.GetDataLimit()), SizeLimit: int(p.GetSizeLimit()), Cross: tri(p.GetCrossNetwork())}
}

// group maps the optional wire filter; a negative index is rejected by the caller as no filter.
func group(g *pb.GroupsFilter) *store.Group {
	if g == nil || g.GetGroupIndex() < 0 {
		return nil
	}
	return &store.Group{Index: int(g.GetGroupIndex()), Value: int(min(g.GetGroupValue(), 1<<20))}
}

func pbBoard(b store.Board) *pb.Board {
	cross := pb.Tristate_TRISTATE_FALSE
	if b.Cross {
		cross = pb.Tristate_TRISTATE_TRUE
	}
	return &pb.Board{Title: b.Title, Id: b.ID, CrossNetwork: b.Cross, Policy: &pb.Policy{
		Name: b.Name, Kind: b.Kind, Order: b.Order, Aggregation: b.Aggregation, ColumnAggregations: b.ColumnAggs,
		ScoreColumn: b.ScoreColumn, Unit: b.Unit, Season: b.Season, RankLimit: uint32(b.RankLimit), DataLimit: uint32(b.DataLimit),
		SizeLimit: uint32(b.SizeLimit), CrossNetwork: cross}}
}

func pbEntry(e store.Entry) *pb.Entry {
	return &pb.Entry{Account: e.Account, Sub: int32(e.Sub), Score: e.Score, Columns: e.Columns, Comment: e.Comment, Info: e.Info, Groups: e.Groups,
		HasData: e.HasData, Network: e.Network, DisplayName: e.DisplayName, UpdatedUnixMs: e.Updated.UnixMilli(), Rank: e.Rank, Position: e.Position}
}

func pbEntries(es []store.Entry) []*pb.Entry {
	out := make([]*pb.Entry, 0, len(es))
	for _, e := range es {
		out = append(out, pbEntry(e))
	}
	return out
}

func (s *Server) Submit(ctx context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	_, res, total, err := s.svc.Submit(ctx, ref(req.GetRef()), req.GetAccount(), int(req.GetSub()), req.GetScore(), req.GetColumns(),
		req.GetComment(), req.GetInfo(), req.GetDisplayName(), policy(req.GetCreatePolicy()), store.SubmitOpts{Groups: req.GetGroups(), AggOverride: req.GetAggregationOverride()})
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.SubmitResponse{Changed: res.Changed, Entry: pbEntry(res.Entry), PreviousScore: res.Previous, HadPrevious: res.HadPrevious, Total: total}, nil
}

func (s *Server) Range(ctx context.Context, req *pb.RangeRequest) (*pb.RangeResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	_, es, total, err := s.svc.Range(ctx, ref(req.GetRef()), req.GetStart(), req.GetLimit(), group(req.GetGroupsFilter()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.RangeResponse{Entries: pbEntries(es), Total: total}, nil
}

func (s *Server) ByAccounts(ctx context.Context, req *pb.ByAccountsRequest) (*pb.RangeResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	_, es, total, err := s.svc.ByAccounts(ctx, ref(req.GetRef()), req.GetAccounts(), group(req.GetGroupsFilter()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.RangeResponse{Entries: pbEntries(es), Total: total}, nil
}

func (s *Server) FriendRanks(ctx context.Context, req *pb.FriendRanksRequest) (*pb.RangeResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	_, es, total, hits, err := s.svc.FriendRanks(ctx, ref(req.GetRef()), req.GetAccount(), req.GetIncludeSelf(), req.GetOffset(), req.GetLimit(), group(req.GetGroupsFilter()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.RangeResponse{Entries: pbEntries(es), Total: total, Hits: hits}, nil
}

func (s *Server) BoardInfo(ctx context.Context, req *pb.BoardInfoRequest) (*pb.BoardInfoResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	b, err := s.svc.BoardInfo(ctx, ref(req.GetRef()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.BoardInfoResponse{Board: pbBoard(b)}, nil
}

func (s *Server) Total(ctx context.Context, req *pb.TotalRequest) (*pb.TotalResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	n, err := s.svc.Total(ctx, ref(req.GetRef()), group(req.GetGroupsFilter()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.TotalResponse{Total: n}, nil
}

func (s *Server) SetData(ctx context.Context, req *pb.SetDataRequest) (*pb.SetDataResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	ok, err := s.svc.SetData(ctx, ref(req.GetRef()), req.GetAccount(), int(req.GetSub()), req.GetExpectScore(), req.GetData())
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.SetDataResponse{Stored: ok}, nil
}

func (s *Server) GetData(ctx context.Context, req *pb.GetDataRequest) (*pb.GetDataResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	d, err := s.svc.GetData(ctx, ref(req.GetRef()), req.GetAccount(), int(req.GetSub()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.GetDataResponse{Found: d != nil, Data: d}, nil
}

func (s *Server) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	n, err := s.svc.Delete(ctx, ref(req.GetRef()), req.GetAccount(), req.GetOneSub(), int(req.GetSub()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.DeleteResponse{Deleted: n}, nil
}

func (s *Server) UpsertBoard(ctx context.Context, req *pb.UpsertBoardRequest) (*pb.BoardInfoResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	b, err := s.svc.UpsertBoard(ctx, ref(req.GetRef()), req.GetTitleName(), policy(req.GetPolicy()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.BoardInfoResponse{Board: pbBoard(b)}, nil
}

func (s *Server) MapTitle(ctx context.Context, req *pb.MapTitleRequest) (*pb.MapTitleResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := s.svc.Store.MapTitle(ctx, req.GetTitle(), req.GetName(), req.GetNetwork(), req.GetNativeId()); err != nil {
		return nil, mapErr(err)
	}
	return &pb.MapTitleResponse{Title: req.GetTitle()}, nil
}

func (s *Server) ResetBoard(ctx context.Context, req *pb.ResetBoardRequest) (*pb.ResetBoardResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	n, err := s.svc.ResetBoard(ctx, ref(req.GetRef()))
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ResetBoardResponse{Deleted: n}, nil
}

func (s *Server) ListBoards(ctx context.Context, req *pb.ListBoardsRequest) (*pb.ListBoardsResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	title := ""
	if req.GetTitle() != "" {
		t, err := s.svc.Store.ResolveTitle(ctx, req.GetNetwork(), req.GetTitle(), false)
		if errors.Is(err, store.ErrNoTitle) {
			return &pb.ListBoardsResponse{}, nil
		}
		if err != nil {
			return nil, mapErr(err)
		}
		title = t
	}
	bs, err := s.svc.Store.Boards(ctx, title, false)
	if err != nil {
		return nil, mapErr(err)
	}
	names, err := s.svc.Store.TitleNames(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := &pb.ListBoardsResponse{}
	seen := map[string]bool{}
	for _, b := range bs {
		out.Boards = append(out.Boards, pbBoard(b))
		if !seen[b.Title] {
			seen[b.Title] = true
			out.Titles = append(out.Titles, &pb.TitleName{Title: b.Title, Name: names[b.Title]})
		}
	}
	return out, nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, service.ErrBadAccount), errors.Is(err, service.ErrBadRequest), errors.Is(err, store.ErrBadPolicy):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, service.ErrTooLarge):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, service.ErrNeedNet), errors.Is(err, store.ErrConflict):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, store.ErrNoTitle), errors.Is(err, store.ErrNoBoard), errors.Is(err, pgx.ErrNoRows):
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, "leaderboards service error")
}
