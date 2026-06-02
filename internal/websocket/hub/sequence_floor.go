package hub

import "context"

type SequenceFloorStore interface {
	Get(ctx context.Context, roomID string) (int64, error)
	SetMax(ctx context.Context, roomID string, seq int64) error
}

type sequenceFloorMessageStore struct {
	base  MessageStore
	floor SequenceFloorStore
}

func (s *sequenceFloorMessageStore) GetLastSequenceNumber(ctx context.Context, roomID string) (int64, error) {
	seq, err := s.base.GetLastSequenceNumber(ctx, roomID)
	if err != nil {
		return 0, err
	}
	floor, err := s.floor.Get(ctx, roomID)
	if err != nil {
		return 0, err
	}
	return max(seq, floor), nil
}

func (s *sequenceFloorMessageStore) SaveMany(ctx context.Context, msgs []*Message) error {
	return s.base.SaveMany(ctx, msgs)
}
