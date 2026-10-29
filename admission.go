package main

import (
	"container/heap"
	"context"
	"net/netip"
	"sync"
	"time"
)

const maxAuthSources = 4096

type authSource struct {
	key          string
	until        time.Time
	count, index int
}
type authExpiry []*authSource

func (h authExpiry) Len() int           { return len(h) }
func (h authExpiry) Less(i, j int) bool { return h[i].until.Before(h[j].until) }
func (h authExpiry) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *authExpiry) Push(value any) {
	item := value.(*authSource)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *authExpiry) Pop() any {
	old := *h
	item := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return item
}

type authLimiter struct {
	mu      sync.Mutex
	sources map[string]*authSource
	expiry  authExpiry
}

func (l *authLimiter) allow(key string, now time.Time, window time.Duration, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sources == nil {
		l.sources = make(map[string]*authSource)
	}
	for len(l.expiry) > 0 && !l.expiry[0].until.After(now) {
		item := heap.Pop(&l.expiry).(*authSource)
		delete(l.sources, item.key)
	}
	item := l.sources[key]
	if item == nil {
		if len(l.sources) >= maxAuthSources {
			return false
		}
		item = &authSource{key: key, until: now.Add(window)}
		l.sources[key] = item
		heap.Push(&l.expiry, item)
	}
	if item.count >= limit {
		return false
	}
	item.count++
	return true
}

type ipPolicyCache struct {
	mu         sync.Mutex
	until      time.Time
	prefixes   []netip.Prefix
	loading    chan struct{}
	generation uint64
}

func (s *Store) invalidateIPPolicy() {
	s.ipPolicy.mu.Lock()
	s.ipPolicy.until = time.Time{}
	s.ipPolicy.generation++
	s.ipPolicy.mu.Unlock()
}
func (s *Store) IPBannedContext(parent context.Context, raw string) bool {
	address, err := trustedPeerAddress(raw)
	if err != nil || parent.Err() != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(parent, 200*time.Millisecond)
	defer cancel()
	for {
		if ctx.Err() != nil {
			return true
		}
		s.ipPolicy.mu.Lock()
		now := time.Now()
		if s.ipPolicy.until.After(now) {
			prefixes := s.ipPolicy.prefixes
			s.ipPolicy.mu.Unlock()
			for _, prefix := range prefixes {
				if prefix.Contains(address) {
					return true
				}
			}
			return false
		}
		if pending := s.ipPolicy.loading; pending != nil {
			s.ipPolicy.mu.Unlock()
			select {
			case <-ctx.Done():
				return true
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		s.ipPolicy.loading = pending
		generation := s.ipPolicy.generation
		s.ipPolicy.mu.Unlock()
		prefixes, until, err := s.loadIPPolicy(ctx, now)
		s.ipPolicy.mu.Lock()
		unchanged := generation == s.ipPolicy.generation
		if err == nil && unchanged {
			s.ipPolicy.prefixes = prefixes
			s.ipPolicy.until = until
		}
		s.ipPolicy.loading = nil
		close(pending)
		s.ipPolicy.mu.Unlock()
		if err != nil || ctx.Err() != nil {
			return true
		}
		if !unchanged {
			continue
		}
	}
}

func (s *Store) loadIPPolicy(ctx context.Context, now time.Time) ([]netip.Prefix, time.Time, error) {
	rows, err := s.policyQueryContext(ctx, `SELECT target,expires FROM sanctions WHERE kind='ip' AND revoked=0 AND (expires=0 OR expires>?)`, now.Unix())
	if err != nil {
		return nil, time.Time{}, err
	}
	prefixes := []netip.Prefix{}
	until := now.Add(time.Second)
	for rows.Next() {
		var target string
		var expires int64
		if err = rows.Scan(&target, &expires); err != nil {
			rows.Close()
			return nil, time.Time{}, err
		}
		prefix, err := normalizePrefix(target)
		if err != nil {
			legacy, legacyErr := netip.ParsePrefix(target)
			if legacyErr == nil && legacy.Addr().Is4In6() && legacy.Bits() < 96 {
				// Preserve the old IPv6 meaning of an existing wide mapped rule;
				// never reinterpret it as an all-IPv4 rule or a global deny.
				prefix = legacy.Masked()
			} else {
				rows.Close()
				return nil, time.Time{}, err
			}
		}
		prefixes = append(prefixes, prefix)
		if expires > 0 && time.Unix(expires, 0).Before(until) {
			until = time.Unix(expires, 0)
		}
	}
	err = rows.Err()
	rows.Close()
	return prefixes, until, err
}
