package wsgateway

import (
	"sync"
	"time"
)

type ticketEntry struct {
	userID string
	timer  *time.Timer
}

type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ticketEntry
}

func NewTicketStore() *TicketStore {
	return &TicketStore{
		tickets: make(map[string]ticketEntry),
	}
}

func (s *TicketStore) Set(ticket, userID string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.tickets[ticket]; ok {
		old.timer.Stop()
	}

	timer := time.AfterFunc(ttl, func() {
		s.mu.Lock()
		delete(s.tickets, ticket)
		s.mu.Unlock()
	})
	s.tickets[ticket] = ticketEntry{userID: userID, timer: timer}
}

func (s *TicketStore) GetAndDelete(ticket string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tickets[ticket]
	if !ok {
		return "", false
	}
	entry.timer.Stop()
	delete(s.tickets, ticket)
	return entry.userID, true
}

func (s *TicketStore) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for ticket, entry := range s.tickets {
		entry.timer.Stop()
		delete(s.tickets, ticket)
	}
}
