package ratelimit

import (
	"hash/maphash"
	"sync"
	"time"
)

const numShards = 64

type bucket struct {
	tokens     float64
	lastUpdate time.Time
}

type shard struct {
	sync.Mutex
	m map[string]*bucket
}

type MemoryLimiter struct {
	rps         float64
	burst       float64
	ttl         time.Duration
	seed        maphash.Seed
	shards      []*shard
	stopJanitor chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

func NewMemory(rps float64, burst int, ttl time.Duration) *MemoryLimiter {
	l := &MemoryLimiter{
		rps:         rps,
		burst:       float64(burst),
		ttl:         ttl,
		seed:        maphash.MakeSeed(),
		shards:      make([]*shard, numShards),
		stopJanitor: make(chan struct{}),
	}

	for i := range numShards {
		l.shards[i] = &shard{
			m: make(map[string]*bucket),
		}
	}

	if ttl > 0 {
		l.wg.Add(1)
		go l.janitor()
	}

	return l
}

func (l *MemoryLimiter) Allow(key string) bool {
	now := time.Now()
	hash := maphash.String(l.seed, key)
	s := l.shards[hash%uint64(len(l.shards))]

	s.Lock()
	defer s.Unlock()

	b, exists := s.m[key]
	if !exists {
		s.m[key] = &bucket{
			tokens:     l.burst - 1,
			lastUpdate: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastUpdate).Seconds()
	newTokens := b.tokens + elapsed*l.rps
	if newTokens > l.burst {
		newTokens = l.burst
	}

	if newTokens >= 1 {
		b.tokens = newTokens - 1
		b.lastUpdate = now
		return true
	}

	return false
}

func (l *MemoryLimiter) Stop() {
	if l.ttl > 0 {
		l.stopOnce.Do(func() {
			close(l.stopJanitor)
		})
		l.wg.Wait()
	}
}

func (l *MemoryLimiter) janitor() {
	defer l.wg.Done()

	interval := max(min(l.ttl/2, 10*time.Minute), time.Second)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopJanitor:
			return
		case now := <-ticker.C:
			l.sweep(now)
		}
	}
}

func (l *MemoryLimiter) sweep(now time.Time) {
	for _, s := range l.shards {
		s.Lock()
		for k, b := range s.m {
			if now.Sub(b.lastUpdate) > l.ttl {
				delete(s.m, k)
			}
		}
		s.Unlock()
	}
}
