// Leaderboards: the neutral board store. Boards are keyed by a title key that is the same on
// every network; entries by account. Console adapters (np-score, x360-xstats, nex, wfc) call it
// over gRPC; the website reads the rankings contract natively. Ports: 20210 HTTP, 20211 gRPC,
// 20212 metrics (docs ports.md, block 20210).
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	emit "github.com/openpak/rewards-emit"
	"google.golang.org/grpc"

	"github.com/openpak/leaderboards/internal/api"
	"github.com/openpak/leaderboards/internal/core"
	"github.com/openpak/leaderboards/internal/grpcapi"
	"github.com/openpak/leaderboards/internal/grpclog"
	"github.com/openpak/leaderboards/internal/httplog"
	"github.com/openpak/leaderboards/internal/service"
	"github.com/openpak/leaderboards/internal/store"
	pb "github.com/openpak/leaderboards/proto/openpak/leaderboards/v1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctx := context.Background()
	st, err := store.Open(ctx, env("LEADERBOARDS_DATABASE_URL", "postgres://postgres:test@127.0.0.1:54329/leaderboards?sslmode=disable"))
	if err != nil {
		log.Fatalf("leaderboards: database: %v", err)
	}
	defer st.Close()
	coreClient, err := core.Dial(env("LEADERBOARDS_CORE_ADDRESS", "127.0.0.1:20001"), os.Getenv("LEADERBOARDS_CORE_INTERNAL_KEY"))
	if err != nil {
		log.Fatalf("leaderboards: %v", err)
	}
	rw := emit.FromEnv("leaderboards") // off unless REWARDS_EVENTS_URL and the key are set
	defer rw.Close(ctx)
	svc := &service.Service{Store: st, Core: coreClient, Rewards: rw}
	key := os.Getenv("LEADERBOARDS_INTERNAL_KEY")

	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: key}).Register(mux)
	httpAddr := env("LEADERBOARDS_HTTP_ADDR", "127.0.0.1:20210")
	go func() {
		log.Printf("leaderboards: http on %s", httpAddr)
		srv := &http.Server{Addr: httpAddr, Handler: httplog.Middleware(mux), ReadHeaderTimeout: 5 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}()

	grpcAddr := env("LEADERBOARDS_GRPC_ADDR", "127.0.0.1:20211")
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("leaderboards: grpc listen: %v", err)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(grpclog.Unary), grpc.MaxRecvMsgSize(8<<20))
	pb.RegisterLeaderboardsServer(gs, grpcapi.New(svc, key))
	go func() {
		log.Printf("leaderboards: grpc on %s", grpcAddr)
		log.Fatal(gs.Serve(lis))
	}()

	// Metrics: none beyond the process yet; the port stays claimed so the block is 20210..20219.
	metricsAddr := env("LEADERBOARDS_METRICS_ADDR", "127.0.0.1:20212")
	go func() {
		m := http.NewServeMux()
		m.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		log.Fatal(http.ListenAndServe(metricsAddr, m))
	}()

	select {}
}
