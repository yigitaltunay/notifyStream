package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"

	"github.com/yaltunay/notifystream/internal/domain"
)

const redisKeyPrefix = "notifystream:delivery:"

type ChannelWaiter interface {
	Wait(ctx context.Context, ch domain.Channel) error
}

func NewChannelWaiter(redisURL string, perSecond int, burst int) (ChannelWaiter, func(), error) {
	if perSecond < 1 {
		perSecond = 100
	}
	if burst < 1 {
		burst = perSecond
	}
	if redisURL == "" {
		return newMemoryWaiter(perSecond, burst), func() {}, nil
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("redis ping (check REDIS_URL and network): %w", err)
	}
	limiter := redis_rate.NewLimiter(rdb)
	lim := redis_rate.Limit{
		Rate:   perSecond,
		Burst:  burst,
		Period: time.Second,
	}
	if lim.Burst < lim.Rate {
		lim.Burst = lim.Rate
	}
	return &redisWaiter{rdb: rdb, limiter: limiter, limit: lim}, func() { _ = rdb.Close() }, nil
}

type memoryWaiter struct {
	by map[domain.Channel]*rate.Limiter
}

func newMemoryWaiter(perSecond, burst int) *memoryWaiter {
	lim := rate.Limit(perSecond)
	m := &memoryWaiter{by: make(map[domain.Channel]*rate.Limiter)}
	for _, ch := range []domain.Channel{domain.ChannelSMS, domain.ChannelEmail, domain.ChannelPush} {
		m.by[ch] = rate.NewLimiter(lim, burst)
	}
	return m
}

func (m *memoryWaiter) Wait(ctx context.Context, ch domain.Channel) error {
	l, ok := m.by[ch]
	if !ok {
		return fmt.Errorf("unknown channel %q", ch)
	}
	return l.Wait(ctx)
}

type redisWaiter struct {
	rdb     *redis.Client
	limiter *redis_rate.Limiter
	limit   redis_rate.Limit
}

func (r *redisWaiter) Wait(ctx context.Context, ch domain.Channel) error {
	key := redisKeyPrefix + string(ch)
	for {
		res, err := r.limiter.Allow(ctx, key, r.limit)
		if err != nil {
			return err
		}
		if res.Allowed > 0 {
			return nil
		}
		retry := res.RetryAfter
		if retry <= 0 {
			retry = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}
