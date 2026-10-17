package ledger

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryStoreCanceledUpdate(t *testing.T) {
	s := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.Update(ctx, "test", func(View) (*Change, error) {
		called = true
		return &Change{Wallet: Wallet{Account: "test", Purchased: 10}}, nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestMemoryStoreEntriesCursor(t *testing.T) {
	s := NewMemoryStore()
	s.entries["a"] = []Entry{{Seq: 1}, {Seq: 3}, {Seq: 5}}
	for _, tt := range []struct {
		before int64
		want   []int64
	}{{0, []int64{5, 3, 1}}, {1, nil}, {3, []int64{1}}, {4, []int64{3, 1}}, {6, []int64{5, 3, 1}}} {
		got, err := s.Entries(context.Background(), "a", tt.before, 10)
		if err != nil || len(got) != len(tt.want) {
			t.Fatalf("before %d: %v %v", tt.before, got, err)
		}
		for i := range got {
			if got[i].Seq != tt.want[i] {
				t.Fatalf("before %d: %v", tt.before, got)
			}
		}
	}
}

func BenchmarkEntriesOlderPage(b *testing.B) {
	s := NewMemoryStore()
	entries := make([]Entry, 1<<16)
	for i := range entries {
		entries[i].Seq = int64(i + 1)
	}
	s.entries["test"] = entries
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Entries(context.Background(), "test", 101, 50); err != nil {
			b.Fatal(err)
		}
	}
}
