// Package bus connects chat instances through Redis: pub/sub announces
// events to every instance, and a sorted set tracks who is online.
//
// Nothing here is durable. Redis pub/sub delivers at most once, so clients
// repair anything they miss from Postgres history.
package bus

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	eventsChannel = "chattie:events"
	presenceKey   = "chattie:presence"
)

type Bus struct {
	rdb *redis.Client
}

// Open does not check that Redis is reachable: chat keeps working from
// Postgres while Redis is down, only live delivery degrades.
func Open(url string) (*Bus, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	// Fail fast so a Redis outage does not stall requests.
	opts.DialTimeout = 2 * time.Second
	opts.MaxRetries = 1
	return &Bus{rdb: redis.NewClient(opts)}, nil
}

func (b *Bus) Close() error { return b.rdb.Close() }

func (b *Bus) Publish(ctx context.Context, event []byte) error {
	return b.rdb.Publish(ctx, eventsChannel, event).Err()
}

// Subscribe calls onEvent for each event until ctx ends. The subscription
// reconnects by itself; events published while it was down are lost, so
// onResync is called after each reconnect to make clients catch up.
func (b *Bus) Subscribe(ctx context.Context, onEvent func([]byte), onResync func()) {
	sub := b.rdb.Subscribe(ctx, eventsChannel)
	go func() {
		<-ctx.Done()
		sub.Close()
	}()

	subscribed := false
	for msg := range sub.ChannelWithSubscriptions() {
		switch msg := msg.(type) {
		case *redis.Subscription:
			if msg.Kind != "subscribe" {
				continue
			}
			if subscribed {
				onResync()
			}
			subscribed = true
		case *redis.Message:
			onEvent([]byte(msg.Payload))
		}
	}
}

// Presence is a sorted set of "username:connectionID" members scored by the
// time their lease expires. A connection that stops renewing simply ages out,
// which covers instances that die without cleaning up.

func (b *Bus) Renew(ctx context.Context, ttl time.Duration, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	expires := float64(time.Now().Add(ttl).Unix())
	leases := make([]redis.Z, len(members))
	for i, m := range members {
		leases[i] = redis.Z{Score: expires, Member: m}
	}
	return b.rdb.ZAdd(ctx, presenceKey, leases...).Err()
}

func (b *Bus) Forget(ctx context.Context, member string) error {
	return b.rdb.ZRem(ctx, presenceKey, member).Err()
}

// Online returns the usernames with at least one live connection.
func (b *Bus) Online(ctx context.Context) ([]string, error) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	b.rdb.ZRemRangeByScore(ctx, presenceKey, "-inf", now) // drop expired leases
	members, err := b.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key: presenceKey, Start: now, Stop: "+inf", ByScore: true,
	}).Result()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(members))
	for i, m := range members {
		names[i], _, _ = strings.Cut(m, ":")
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}
