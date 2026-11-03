// Package core is the leaderboards service's client of the account core: the friend list (the
// one answer for every network's friend ranking) and the universal event stream a new record is
// announced on. Leaderboards never stores a friendship and never invents an event the core did
// not countersign.
package core

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
)

type Client struct {
	social accountv1.SocialClient
	events accountv1.EventsClient
	key    string
}

// Dial opens the core connection. Insecure on purpose: the internal network is the boundary.
func Dial(addr, internalKey string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("leaderboards: core dial: %w", err)
	}
	return New(accountv1.NewSocialClient(conn), accountv1.NewEventsClient(conn), internalKey), nil
}

// New builds a client on existing stubs; tests hand it bufconn-backed ones.
func New(social accountv1.SocialClient, events accountv1.EventsClient, internalKey string) *Client {
	return &Client{social: social, events: events, key: internalKey}
}

func (c *Client) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-internal-key", c.key)
}

// Friends lists the account's friends (account ids) from the core's graph.
func (c *Client) Friends(ctx context.Context, account string) ([]string, error) {
	rctx, cancel := context.WithTimeout(c.ctx(ctx), 5*time.Second)
	defer cancel()
	res, err := c.social.ListFriends(rctx, &accountv1.ListFriendsRequest{AccountId: account})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.GetFriends()))
	for _, f := range res.GetFriends() {
		out = append(out, f.GetAccountId())
	}
	return out, nil
}

// Announce places "leaderboard.record" on the core's stream: account_id = the player,
// subject_id = "<title>/<board>". Scores never ride the event; readers fetch them here.
func (c *Client) Announce(ctx context.Context, account, subject string) error {
	rctx, cancel := context.WithTimeout(c.ctx(ctx), 5*time.Second)
	defer cancel()
	_, err := c.events.EmitEvent(rctx, &accountv1.EmitEventRequest{Type: "leaderboard.record", AccountId: account, SubjectId: subject})
	return err
}
