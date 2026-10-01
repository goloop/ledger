package ledger

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// MemoryStore is a Store held in memory. It is the reference implementation
// of the contract, and it is what tests and single-process tools use.
//
// Nothing is shared between processes and nothing survives a restart. A
// service needs a store backed by its database; ledgertest.Store checks one
// against the same rules this one follows.
//
// Every method is safe for concurrent use. Update holds one lock for the
// whole read-decide-write, which serializes every account rather than each
// account separately - simpler than a lock per account, and the throughput
// of a test does not need more.
type MemoryStore struct {
	mu      sync.Mutex
	wallets map[string]Wallet
	entries map[string][]Entry         // by account, oldest first
	keys    map[string]map[string]int  // account -> key -> index into entries
	holds   map[string]map[string]Hold // account -> id -> hold
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		wallets: map[string]Wallet{},
		entries: map[string][]Entry{},
		keys:    map[string]map[string]int{},
		holds:   map[string]map[string]Hold{},
	}
}

// Wallet implements Store.
func (s *MemoryStore) Wallet(_ context.Context, account string) (Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wallets[account]
	if !ok {
		return Wallet{Account: account}, nil
	}
	return w, nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, account string, fn func(View) (*Change, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.wallets[account]
	if !ok {
		w = Wallet{Account: account}
	}
	ch, err := fn(memView{s: s, account: account, w: w})
	if err != nil || ch == nil {
		return err
	}

	// Check everything before writing anything, so a rejected change leaves
	// the store exactly as it was.
	seen := map[string]bool{}
	for _, e := range ch.Entries {
		if _, dup := s.keys[account][e.Key]; dup || seen[e.Key] {
			return ErrDuplicateKey
		}
		seen[e.Key] = true
	}

	if s.keys[account] == nil {
		s.keys[account] = map[string]int{}
	}
	for _, e := range ch.Entries {
		s.keys[account][e.Key] = len(s.entries[account])
		s.entries[account] = append(s.entries[account], e)
	}
	if len(ch.Holds) > 0 && s.holds[account] == nil {
		s.holds[account] = map[string]Hold{}
	}
	for _, h := range ch.Holds {
		s.holds[account][h.ID] = h
	}
	s.wallets[account] = ch.Wallet
	return nil
}

// Entries implements Store.
func (s *MemoryStore) Entries(_ context.Context, account string, before int64, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.entries[account]
	out := make([]Entry, 0, max(0, min(limit, len(all))))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		if before > 0 && all[i].Seq >= before {
			continue
		}
		out = append(out, all[i])
	}
	return out, nil
}

// StaleHolds implements Store.
func (s *MemoryStore) StaleHolds(_ context.Context, createdBefore time.Time, limit int) ([]Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Hold
	for _, byID := range s.holds {
		for _, h := range byID {
			if h.Status == HoldHeld && h.CreatedAt.Before(createdBefore) {
				out = append(out, h)
			}
		}
	}
	slices.SortFunc(out, compareHolds)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// memView reads the locked state; the store's mutex is held while it is used.
type memView struct {
	s       *MemoryStore
	account string
	w       Wallet
}

func (v memView) Wallet() Wallet { return v.w }

func (v memView) Entry(_ context.Context, key string) (Entry, bool, error) {
	i, ok := v.s.keys[v.account][key]
	if !ok {
		return Entry{}, false, nil
	}
	return v.s.entries[v.account][i], true, nil
}

func (v memView) Hold(_ context.Context, id string) (Hold, bool, error) {
	h, ok := v.s.holds[v.account][id]
	return h, ok, nil
}

func (v memView) Held(context.Context) ([]Hold, error) {
	var out []Hold
	for _, h := range v.s.holds[v.account] {
		if h.Status == HoldHeld {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, compareHolds)
	return out, nil
}

// compareHolds orders holds oldest first, by id when two were created at the
// same instant, so the order never depends on map iteration.
func compareHolds(a, b Hold) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}
