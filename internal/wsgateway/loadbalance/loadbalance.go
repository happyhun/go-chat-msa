package loadbalance

import (
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
	found := r.hash.LocateKey([]byte(roomID))
	if found == nil {
		return ""
	}
	return found.String()
}

func (h hasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}

func (m member) String() string {
	return string(m)
}
