package loadbalance

import (
	"sync"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash/v2"
)

const (
	defaultPartitionCount    = 71
	defaultReplicationFactor = 20
	defaultLoad              = 1.25
)

type hasher struct{}

type member string

type HashRing struct {
	mu   sync.RWMutex
	hash *consistent.Consistent
}

func New(endpoints []string) *HashRing {
	cfg := consistent.Config{
		PartitionCount:    defaultPartitionCount,
		ReplicationFactor: defaultReplicationFactor,
		Load:              defaultLoad,
		Hasher:            hasher{},
	}

	inst := consistent.New(nil, cfg)
	for _, endpoint := range endpoints {
		inst.Add(member(endpoint))
	}

	return &HashRing{hash: inst}
}

func (r *HashRing) Locate(roomID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	found := r.hash.LocateKey([]byte(roomID))
	if found == nil {
		return ""
	}
	return found.String()
}

func (r *HashRing) Set(addrs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		desired[a] = struct{}{}
	}

	current := make(map[string]struct{})
	for _, m := range r.hash.GetMembers() {
		current[m.String()] = struct{}{}
	}

	for a := range desired {
		if _, ok := current[a]; !ok {
			r.hash.Add(member(a))
		}
	}
	for a := range current {
		if _, ok := desired[a]; !ok {
			r.hash.Remove(a)
		}
	}
}

func (r *HashRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hash.GetMembers())
}

func (r *HashRing) Contains(addr string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.hash.GetMembers() {
		if m.String() == addr {
			return true
		}
	}
	return false
}

func (h hasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}

func (m member) String() string {
	return string(m)
}
