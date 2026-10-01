package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goloop/ledger"
)

var ctx = context.Background()

var t0 = time.Date(2030, 3, 10, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newLedger(t *testing.T) (*ledger.Ledger, *ledger.MemoryStore, *clock) {
	t.Helper()
	s := ledger.NewMemoryStore()
	c := &clock{now: t0}
	return ledger.New(s, ledger.WithClock(c.Now)), s, c
}

func balance(t *testing.T, l *ledger.Ledger, account string) ledger.Wallet {
	t.Helper()
	w, err := l.Balance(ctx, account)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	return w
}

func want(t *testing.T, l *ledger.Ledger, account string, bonus, purchased int64) {
	t.Helper()
	w := balance(t, l, account)
	if w.Bonus != bonus || w.Purchased != purchased {
		t.Fatalf("wallet %q: want %d/%d, got %d/%d", account, bonus, purchased, w.Bonus, w.Purchased)
	}
	if err := l.Audit(ctx, account); err != nil {
		t.Fatalf("Audit: %v", err)
	}
}

func entries(t *testing.T, l *ledger.Ledger, account string) []ledger.Entry {
	t.Helper()
	es, err := l.Entries(ctx, account, 0, 1000)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	return es
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func month() time.Time { return t0.Add(30 * 24 * time.Hour) }

func TestValidation(t *testing.T) {
	l, _, _ := newLedger(t)
	long := strings.Repeat("k", ledger.MaxKeyLength+1)
	cases := map[string]error{
		"empty account":   l.Deposit(ctx, "", ledger.Deposit{Key: "k", Amount: 1}),
		"empty key":       l.Deposit(ctx, "a", ledger.Deposit{Amount: 1}),
		"long key":        l.Deposit(ctx, "a", ledger.Deposit{Key: long, Amount: 1}),
		"reserved key":    l.Deposit(ctx, "a", ledger.Deposit{Key: "ledger:x", Amount: 1}),
		"zero deposit":    l.Deposit(ctx, "a", ledger.Deposit{Key: "k", Amount: 0}),
		"negative grant":  l.Grant(ctx, "a", ledger.Grant{Key: "k", Amount: -1, Until: month()}),
		"grant no expiry": l.Grant(ctx, "a", ledger.Grant{Key: "k", Amount: 1}),
		"grant past":      l.Grant(ctx, "a", ledger.Grant{Key: "k", Amount: 1, Until: t0}),
		"grant origin":    l.Grant(ctx, "a", ledger.Grant{Key: "k", Amount: 1, Until: month(), Origin: "gift"}),
		"zero spend":      l.Spend(ctx, "a", ledger.Charge{Key: "k"}),
		"empty adjust":    l.Adjust(ctx, "a", ledger.Adjustment{Key: "k"}),
		"adjust no until": l.Adjust(ctx, "a", ledger.Adjustment{Key: "k", Bonus: 5}),
		"capture no id":   l.Capture(ctx, "a", ""),
	}
	_, holdErr := l.Hold(ctx, "a", ledger.Charge{Key: "k", Amount: -5})
	cases["negative hold"] = holdErr
	_, revErr := l.Reverse(ctx, "a", ledger.Reversal{Key: "k"})
	cases["zero reversal"] = revErr
	_, staleErr := l.ReleaseStale(ctx, 0, 10)
	cases["stale age"] = staleErr

	for name, err := range cases {
		if !errors.Is(err, ledger.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if es := entries(t, l, "a"); len(es) != 0 {
		t.Fatalf("a refused request must write nothing, got %d entries", len(es))
	}
}

func TestDepositIsIdempotent(t *testing.T) {
	l, _, _ := newLedger(t)
	for range 3 {
		mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 1000, Ref: "txn_1"}))
	}
	want(t, l, "a", 0, 1000)
	if n := len(entries(t, l, "a")); n != 1 {
		t.Fatalf("three deliveries of one payment must make one entry, got %d", n)
	}

	err := l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 2000, Ref: "txn_1"})
	if !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a key reused for another amount must be refused, got %v", err)
	}
	err = l.Grant(ctx, "a", ledger.Grant{Key: "txn_1", Amount: 1000, Until: month()})
	if !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a key reused for another kind must be refused, got %v", err)
	}
	want(t, l, "a", 0, 1000)
}

func TestGrantExtendsAndReplaces(t *testing.T) {
	l, _, c := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "welcome", Amount: 300, Until: t0.Add(10 * 24 * time.Hour)}))
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "early", Amount: 50, Until: t0.Add(5 * 24 * time.Hour)}))
	w := balance(t, l, "a")
	if w.Bonus != 350 || !w.BonusExpiresAt.Equal(t0.Add(10*24*time.Hour)) {
		t.Fatalf("a grant with an earlier expiry must not shorten the bucket's: got %d until %s", w.Bonus, w.BonusExpiresAt)
	}

	mustOK(t, l.Grant(ctx, "a", ledger.Grant{
		Key: "plan:2030-03", Amount: 1000, Until: t0.Add(3 * 24 * time.Hour), Replace: true, Origin: ledger.OriginPlan,
	}))
	w = balance(t, l, "a")
	if w.Bonus != 1000 || !w.BonusExpiresAt.Equal(t0.Add(3*24*time.Hour)) {
		t.Fatalf("Replace must expire the leftover and set the new expiry: got %d until %s", w.Bonus, w.BonusExpiresAt)
	}
	es := entries(t, l, "a")
	if es[0].Kind != ledger.KindGrant || es[0].Origin != ledger.OriginPlan || es[1].Kind != ledger.KindExpire || es[1].Bonus != -350 {
		t.Fatalf("Replace must record the write-off before the grant, got %+v", es[:2])
	}
	if es[2].Origin != ledger.OriginPromo {
		t.Fatalf("a grant without an origin is promo, got %q", es[2].Origin)
	}
	c.Add(time.Hour)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{
		Key: "plan:2030-03", Amount: 1000, Until: t0.Add(3 * 24 * time.Hour), Replace: true, Origin: ledger.OriginPlan,
	}))
	want(t, l, "a", 1000, 0)
}

func TestBonusExpires(t *testing.T) {
	l, _, c := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 300, Until: t0.Add(24 * time.Hour)}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	c.Add(24 * time.Hour)

	w := balance(t, l, "a")
	if w.Bonus != 0 || !w.BonusExpiresAt.IsZero() || w.Purchased != 100 {
		t.Fatalf("Balance must show an expired bonus as empty, got %+v", w)
	}
	if n := len(entries(t, l, "a")); n != 2 {
		t.Fatalf("Balance only reads: want 2 entries, got %d", n)
	}

	_, err := l.Hold(ctx, "a", ledger.Charge{Key: "h", Amount: 150})
	var ie *ledger.InsufficientError
	if !errors.As(err, &ie) || !errors.Is(err, ledger.ErrInsufficient) || ie.Required != 150 || ie.Available != 100 {
		t.Fatalf("expired bonus credits cannot pay: want need 150 have 100, got %v", err)
	}

	mustOK(t, l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: 10}))
	es := entries(t, l, "a")
	if es[1].Kind != ledger.KindExpire || es[1].Bonus != -300 || es[1].BonusAfter != 0 {
		t.Fatalf("the next movement must write the expiry off first, got %+v", es[1])
	}
	want(t, l, "a", 0, 90)
}

func TestHoldCaptureRelease(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 30, Until: month()}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))

	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 50, Ref: "pdf"})
	mustOK(t, err)
	if h.FromBonus != 30 || h.FromPurchased != 20 || h.Status != ledger.HoldHeld || h.ID == "" || h.Account != "a" {
		t.Fatalf("a hold spends bonus first: want 30+20, got %+v", h)
	}
	want(t, l, "a", 0, 80)

	again, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 50, Ref: "pdf"})
	mustOK(t, err)
	if again.ID != h.ID {
		t.Fatalf("a repeated Hold must return the first hold, got %q want %q", again.ID, h.ID)
	}
	if _, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 60, Ref: "pdf"}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a repeated Hold with another amount must be refused, got %v", err)
	}
	want(t, l, "a", 0, 80)

	mustOK(t, l.Release(ctx, "a", h.ID))
	want(t, l, "a", 30, 100)
	mustOK(t, l.Release(ctx, "a", h.ID))
	want(t, l, "a", 30, 100)
	if err := l.Capture(ctx, "a", h.ID); !errors.Is(err, ledger.ErrReleased) {
		t.Fatalf("capturing a released hold must say so, got %v", err)
	}

	h2, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-2", Amount: 40})
	mustOK(t, err)
	mustOK(t, l.Capture(ctx, "a", h2.ID))
	mustOK(t, l.Capture(ctx, "a", h2.ID))
	mustOK(t, l.Release(ctx, "a", h2.ID)) // the deferred release after a capture
	want(t, l, "a", 0, 90)

	got, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-2", Amount: 40})
	mustOK(t, err)
	if got.Status != ledger.HoldCaptured || got.SettledAt.IsZero() {
		t.Fatalf("a repeated Hold returns the hold as it is now, got %+v", got)
	}

	if err := l.Capture(ctx, "a", "no-such-hold"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("an unknown hold must be ErrNotFound, got %v", err)
	}
	if err := l.Capture(ctx, "b", h2.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("another account's hold must be ErrNotFound, got %v", err)
	}

	kinds := []ledger.Kind{}
	for _, e := range entries(t, l, "a") {
		kinds = append(kinds, e.Kind)
	}
	wantKinds := []ledger.Kind{ledger.KindCapture, ledger.KindHold, ledger.KindRelease, ledger.KindHold, ledger.KindDeposit, ledger.KindGrant}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Fatalf("each settlement must be recorded once: want %v, got %v", wantKinds, kinds)
	}
}

func TestReleaseAfterBonusExpired(t *testing.T) {
	l, _, c := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 30, Until: t0.Add(time.Hour)}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 30})
	mustOK(t, err)

	c.Add(2 * time.Hour)
	mustOK(t, l.Release(ctx, "a", h.ID))
	w := balance(t, l, "a")
	if w.Bonus != 30 || !w.BonusExpiresAt.Equal(c.Now().Add(ledger.DefaultReleaseGrace)) {
		t.Fatalf("bonus credits a failed action brings back after their expiry get the grace: got %d until %s",
			w.Bonus, w.BonusExpiresAt)
	}

	c.Add(ledger.DefaultReleaseGrace)
	want(t, l, "a", 0, 0)
}

func TestReleaseKeepsExpiry(t *testing.T) {
	l, _, _ := newLedger(t)
	exp := t0.Add(48 * time.Hour)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 30, Until: exp}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 30})
	mustOK(t, err)
	mustOK(t, l.Release(ctx, "a", h.ID))
	if w := balance(t, l, "a"); !w.BonusExpiresAt.Equal(exp) {
		t.Fatalf("bonus credits spent down to zero and brought back keep their expiry, got %s", w.BonusExpiresAt)
	}
}

func TestSpend(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 5, Until: month()}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 10}))
	mustOK(t, l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: 8, Ref: "tts"}))
	mustOK(t, l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: 8, Ref: "tts"}))
	want(t, l, "a", 0, 7)
	if err := l.Spend(ctx, "a", ledger.Charge{Key: "s2", Amount: 8}); !errors.Is(err, ledger.ErrInsufficient) {
		t.Fatalf("want ErrInsufficient, got %v", err)
	}
	if err := l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: 8, Ref: "cover"}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a key reused for another feature must be refused, got %v", err)
	}
}

func TestReverse(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 50, Until: month()}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 1000}))
	mustOK(t, l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: 450}))

	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj_1", Amount: 1000, Ref: "txn_1", Note: "chargeback"})
	mustOK(t, err)
	if taken != 600 {
		t.Fatalf("a reversal takes what is left of the purchased credits: want 600, got %d", taken)
	}
	want(t, l, "a", 0, 0)
	if e := entries(t, l, "a")[0]; e.Requested != 1000 || e.Taken() != 600 || e.Short() != 400 || e.Note != "chargeback" {
		t.Fatalf("the shortfall must be recorded as numbers, got requested %d taken %d short %d note %q",
			e.Requested, e.Taken(), e.Short(), e.Note)
	}

	again, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj_1", Amount: 1000, Ref: "txn_1"})
	mustOK(t, err)
	if again != 600 {
		t.Fatalf("a repeated reversal reports what the first took, got %d", again)
	}
	if _, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj_1", Amount: 500, Ref: "txn_1"}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a reversal key reused for another amount must be refused, got %v", err)
	}

	mustOK(t, l.Grant(ctx, "b", ledger.Grant{Key: "g", Amount: 50, Until: month()}))
	taken, err = l.Reverse(ctx, "b", ledger.Reversal{Key: "adj_2", Amount: 10})
	mustOK(t, err)
	if taken != 0 {
		t.Fatalf("a reversal never reaches bonus credits, took %d", taken)
	}
	want(t, l, "b", 50, 0)
}

func TestAdjust(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-1", Purchased: 100, Note: "support ticket 42"}))
	mustOK(t, l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-1", Purchased: 100, Note: "support ticket 42"}))
	mustOK(t, l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-2", Bonus: 20, BonusUntil: month()}))
	mustOK(t, l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-3", Bonus: 5}))
	mustOK(t, l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-4", Purchased: -30}))
	want(t, l, "a", 25, 70)

	var ie *ledger.InsufficientError
	if err := l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-5", Purchased: -71}); !errors.As(err, &ie) || ie.Available != 70 {
		t.Fatalf("an adjustment cannot take a bucket below zero, got %v", err)
	}
	if err := l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-6", Bonus: -26}); !errors.Is(err, ledger.ErrInsufficient) {
		t.Fatalf("an adjustment cannot take the bonus below zero, got %v", err)
	}
	if err := l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-1", Purchased: 101}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("want ErrKeyReused, got %v", err)
	}
	if err := l.Adjust(ctx, "a", ledger.Adjustment{Key: "fix-7", Bonus: 1, BonusUntil: t0}); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("an expiry in the past is invalid, got %v", err)
	}
}

func TestReleaseStale(t *testing.T) {
	l, _, c := newLedger(t)
	for _, acc := range []string{"a", "b"} {
		mustOK(t, l.Deposit(ctx, acc, ledger.Deposit{Key: "d", Amount: 100}))
	}
	old, err := l.Hold(ctx, "a", ledger.Charge{Key: "old", Amount: 10})
	mustOK(t, err)
	oldB, err := l.Hold(ctx, "b", ledger.Charge{Key: "old", Amount: 20})
	mustOK(t, err)
	done, err := l.Hold(ctx, "a", ledger.Charge{Key: "done", Amount: 5})
	mustOK(t, err)
	mustOK(t, l.Capture(ctx, "a", done.ID))
	c.Add(2 * time.Hour)
	_, err = l.Hold(ctx, "a", ledger.Charge{Key: "fresh", Amount: 1})
	mustOK(t, err)

	n, err := l.ReleaseStale(ctx, time.Hour, 0)
	mustOK(t, err)
	if n != 2 {
		t.Fatalf("two holds are stale, released %d", n)
	}
	want(t, l, "a", 0, 94)
	want(t, l, "b", 0, 100)
	if err := l.Capture(ctx, "a", old.ID); !errors.Is(err, ledger.ErrReleased) {
		t.Fatalf("a late capture of a stale hold must say it was released, got %v", err)
	}
	_ = oldB
	if n, _ := l.ReleaseStale(ctx, time.Hour, 0); n != 0 {
		t.Fatalf("a second sweep has nothing to release, released %d", n)
	}
}

func TestConcurrentHoldsNeverOverdraw(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 500}))

	var ok, short atomic.Int64
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Hold(ctx, "a", ledger.Charge{Key: fmt.Sprint("job-", i), Amount: 10})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficient):
				short.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 50 || short.Load() != 50 {
		t.Fatalf("500 credits cover exactly 50 holds of 10: got %d held, %d refused", ok.Load(), short.Load())
	}
	want(t, l, "a", 0, 0)
}

// retryingStore calls fn twice on every update and keeps the second result,
// as a database store does when it retries an aborted transaction.
type retryingStore struct {
	*ledger.MemoryStore
	calls atomic.Int64
}

func (s *retryingStore) Update(ctx context.Context, account string, fn func(ledger.View) (*ledger.Change, error)) error {
	return s.MemoryStore.Update(ctx, account, func(v ledger.View) (*ledger.Change, error) {
		s.calls.Add(1)
		if _, err := fn(v); err != nil {
			return nil, err
		}
		return fn(v)
	})
}

func TestUpdateFunctionCanBeRetried(t *testing.T) {
	s := &retryingStore{MemoryStore: ledger.NewMemoryStore()}
	l := ledger.New(s, ledger.WithClock(func() time.Time { return t0 }))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 40})
	mustOK(t, err)
	mustOK(t, l.Capture(ctx, "a", h.ID))
	want(t, l, "a", 0, 60)
	if es := entries(t, l, "a"); len(es) != 3 {
		t.Fatalf("a retried update must not double its entries, got %d", len(es))
	}
}

func TestAuditFindsCorruption(t *testing.T) {
	l, s, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	mustOK(t, l.Audit(ctx, "a"))

	// A wallet written behind the ledger's back.
	err := s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		w := v.Wallet()
		w.Purchased += 1000
		return &ledger.Change{Wallet: w}, nil
	})
	mustOK(t, err)
	if err := l.Audit(ctx, "a"); !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a wallet that disagrees with its history must fail the audit, got %v", err)
	}

	// An entry whose recorded balance does not follow from the one before.
	err = s.Update(ctx, "b", func(v ledger.View) (*ledger.Change, error) {
		return &ledger.Change{
			Wallet: ledger.Wallet{Account: "b", Purchased: 50, Seq: 2},
			Entries: []ledger.Entry{
				{Account: "b", Seq: 1, Kind: ledger.KindDeposit, Key: "x", Purchased: 10, PurchasedAfter: 10},
				{Account: "b", Seq: 2, Kind: ledger.KindDeposit, Key: "y", Purchased: 10, PurchasedAfter: 50},
			},
		}, nil
	})
	mustOK(t, err)
	if err := l.Audit(ctx, "b"); !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a recorded balance that does not add up must fail the audit, got %v", err)
	}

	// A gap in the numbering.
	err = s.Update(ctx, "c", func(v ledger.View) (*ledger.Change, error) {
		return &ledger.Change{
			Wallet:  ledger.Wallet{Account: "c", Purchased: 10, Seq: 2},
			Entries: []ledger.Entry{{Account: "c", Seq: 2, Kind: ledger.KindDeposit, Key: "x", Purchased: 10, PurchasedAfter: 10}},
		}, nil
	})
	mustOK(t, err)
	if err := l.Audit(ctx, "c"); !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a missing entry must fail the audit, got %v", err)
	}
}

func TestAuditPagesThroughLongHistory(t *testing.T) {
	l, _, _ := newLedger(t)
	for i := range 2503 {
		mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: fmt.Sprint("d", i), Amount: 1}))
	}
	want(t, l, "a", 0, 2503)
}

func TestEntriesPaging(t *testing.T) {
	l, _, _ := newLedger(t)
	for i := range 5 {
		mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: fmt.Sprint("d", i), Amount: 1}))
	}
	page, err := l.Entries(ctx, "a", 0, 2)
	mustOK(t, err)
	if len(page) != 2 || page[0].Seq != 5 {
		t.Fatalf("want the two newest, got %+v", page)
	}
	page, err = l.Entries(ctx, "a", page[1].Seq, 0)
	mustOK(t, err)
	if len(page) != 3 || page[0].Seq != 3 || page[2].Seq != 1 {
		t.Fatalf("want 3, 2, 1, got %+v", page)
	}
	if _, err := l.Entries(ctx, "", 0, 1); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}
