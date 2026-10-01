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

func TestReverseReachesHoldsInFlight(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 50, Until: month()}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn", Amount: 1000}))
	h1, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 650}) // 50 bonus + 600 purchased
	mustOK(t, err)
	h2, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-2", Amount: 300})
	mustOK(t, err)
	want(t, l, "a", 0, 100)

	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj", Amount: 1000, Ref: "txn"})
	mustOK(t, err)
	if taken != 1000 {
		t.Fatalf("a reversal must reach the purchased credits in holds: want 1000, got %d", taken)
	}
	e := entries(t, l, "a")[0]
	if e.Purchased != -100 || e.FromHolds != 900 || e.Requested != 1000 || e.Short() != 0 {
		t.Fatalf("the reversal entry must say where the credits came from, got %+v", e)
	}
	want(t, l, "a", 0, 0)

	// The failed actions get back only what the refund did not take: the
	// bonus share, nothing purchased.
	mustOK(t, l.Release(ctx, "a", h1.ID))
	mustOK(t, l.Release(ctx, "a", h2.ID))
	want(t, l, "a", 50, 0)

	again, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj", Amount: 1000, Ref: "txn"})
	mustOK(t, err)
	if again != 1000 {
		t.Fatalf("a repeated reversal reports what the first took, holds included; got %d", again)
	}
	want(t, l, "a", 50, 0)
}

func TestReverseThenCapture(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn", Amount: 100}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 100})
	mustOK(t, err)
	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj", Amount: 40})
	mustOK(t, err)
	if taken != 40 {
		t.Fatalf("want 40 from the hold, got %d", taken)
	}
	h2, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 100})
	if !errors.Is(err, ledger.ErrHoldExists) || h2.ID != h.ID || h2.Reversed != 40 {
		t.Fatalf("the hold must record what the reversal took, got %+v", h2)
	}
	// The action ran: the hold is spent as it is, nothing comes back.
	mustOK(t, l.Capture(ctx, "a", h.ID))
	want(t, l, "a", 0, 0)
}

func TestReverseExact(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn", Amount: 100}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 10})
	mustOK(t, err)

	var ie *ledger.InsufficientError
	_, err = l.Reverse(ctx, "a", ledger.Reversal{Key: "withdraw", Amount: 100, Exact: true})
	if !errors.As(err, &ie) || ie.Required != 100 || ie.Available != 90 {
		t.Fatalf("an exact reversal of an unused pack must fail when part of it is used, got %v", err)
	}
	if n := len(entries(t, l, "a")); n != 2 {
		t.Fatalf("a refused exact reversal writes nothing, got %d entries", n)
	}
	mustOK(t, l.Release(ctx, "a", h.ID))
	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "withdraw", Amount: 100, Exact: true})
	mustOK(t, err)
	if taken != 100 {
		t.Fatalf("want 100, got %d", taken)
	}
	want(t, l, "a", 0, 0)
}

func TestHoldAfterReleaseSaysSo(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 40})
	mustOK(t, err)
	mustOK(t, l.Release(ctx, "a", h.ID))

	again, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 40})
	if !errors.Is(err, ledger.ErrReleased) {
		t.Fatalf("a repeated Hold of a released hold must say it was released, got %v", err)
	}
	if again.ID != h.ID || again.Status != ledger.HoldReleased {
		t.Fatalf("the released hold comes back with the error, got %+v", again)
	}
	want(t, l, "a", 0, 100)
}

// raceStore makes one movement of the account while the audit reads the
// history, the way a live system does.
type raceStore struct {
	*ledger.MemoryStore
	l     *ledger.Ledger
	armed bool
}

func (s *raceStore) Entries(c context.Context, account string, before int64, limit int) ([]ledger.Entry, error) {
	if s.armed {
		s.armed = false
		if err := s.l.Deposit(c, account, ledger.Deposit{Key: "late", Amount: 1}); err != nil {
			return nil, err
		}
	}
	return s.MemoryStore.Entries(c, account, before, limit)
}

func TestAuditDuringMovement(t *testing.T) {
	s := &raceStore{MemoryStore: ledger.NewMemoryStore()}
	l := ledger.New(s)
	s.l = l
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	s.armed = true
	if err := l.Audit(ctx, "a"); err != nil {
		t.Fatalf("a movement during the audit must not look like corruption, got %v", err)
	}
}

// staleWallet hides the latest wallet update, as a store that wrote the
// entry and lost the wallet would.
type staleWallet struct {
	*ledger.MemoryStore
	w ledger.Wallet
}

func (s *staleWallet) Wallet(context.Context, string) (ledger.Wallet, error) { return s.w, nil }

func TestAuditFindsStaleWallet(t *testing.T) {
	ms := ledger.NewMemoryStore()
	l := ledger.New(ms)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d1", Amount: 100}))
	w, _ := ms.Wallet(ctx, "a")
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d2", Amount: 100}))
	err := ledger.New(&staleWallet{MemoryStore: ms, w: w}).Audit(ctx, "a")
	if !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a wallet behind its history must fail the audit, got %v", err)
	}
}

func TestAuditFindsHistoryNotFromZero(t *testing.T) {
	l, s, _ := newLedger(t)
	err := s.Update(ctx, "a", func(ledger.View) (*ledger.Change, error) {
		return &ledger.Change{
			Wallet:  ledger.Wallet{Account: "a", Purchased: 20, Seq: 1},
			Entries: []ledger.Entry{{Account: "a", Seq: 1, Kind: ledger.KindDeposit, Key: "x", Purchased: 10, PurchasedAfter: 20}},
		}, nil
	})
	mustOK(t, err)
	if err := l.Audit(ctx, "a"); !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a first entry that does not start from zero must fail the audit, got %v", err)
	}
	mustOK(t, l.Audit(ctx, "nobody"))
	if err := l.Audit(ctx, ""); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestAmountLimits(t *testing.T) {
	l, _, _ := newLedger(t)
	over := int64(ledger.MaxAmount + 1)
	for name, err := range map[string]error{
		"deposit": l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: over}),
		"grant":   l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: over, Until: month()}),
		"spend":   l.Spend(ctx, "a", ledger.Charge{Key: "s", Amount: over}),
		"adjust":  l.Adjust(ctx, "a", ledger.Adjustment{Key: "x", Purchased: -over}),
	} {
		if !errors.Is(err, ledger.ErrInvalid) {
			t.Errorf("%s beyond MaxAmount: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "r", Amount: over}); !errors.Is(err, ledger.ErrInvalid) {
		t.Errorf("reversal beyond MaxAmount: want ErrInvalid, got %v", err)
	}

	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d1", Amount: ledger.MaxAmount}))
	err := l.Deposit(ctx, "a", ledger.Deposit{Key: "d2", Amount: 1})
	if !errors.Is(err, ledger.ErrInvalid) || errors.Is(err, ledger.ErrInsufficient) {
		t.Fatalf("a deposit that would overflow the bucket is invalid, not insufficient; got %v", err)
	}
	want(t, l, "a", 0, ledger.MaxAmount)
}

func TestGrantReplayComparesOrigin(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 10, Until: month(), Origin: ledger.OriginPlan}))
	err := l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 10, Until: month()})
	if !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a grant key reused for another origin must be refused, got %v", err)
	}
}

// failingAccount fails every update of one account.
type failingAccount struct {
	*ledger.MemoryStore
	bad string
}

func (s *failingAccount) Update(c context.Context, account string, fn func(ledger.View) (*ledger.Change, error)) error {
	if account == s.bad {
		return errors.New("db: row locked")
	}
	return s.MemoryStore.Update(c, account, fn)
}

func TestReleaseStaleGoesPastAFailure(t *testing.T) {
	s := &failingAccount{MemoryStore: ledger.NewMemoryStore()}
	now := t0
	l := ledger.New(s, ledger.WithClock(func() time.Time { return now }))
	for _, acc := range []string{"a", "b", "c"} {
		mustOK(t, l.Deposit(ctx, acc, ledger.Deposit{Key: "d", Amount: 10}))
		_, err := l.Hold(ctx, acc, ledger.Charge{Key: "h", Amount: 10})
		mustOK(t, err)
		now = now.Add(time.Second)
	}
	now = now.Add(time.Hour)
	s.bad = "a"
	n, err := l.ReleaseStale(ctx, time.Minute, 10)
	if n != 2 || err == nil {
		t.Fatalf("the oldest hold failing must not stop the sweep: want 2 released and an error, got %d, %v", n, err)
	}
	s.bad = ""
	n, err = l.ReleaseStale(ctx, time.Minute, 10)
	if n != 1 || err != nil {
		t.Fatalf("the next sweep releases the one left, got %d, %v", n, err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	mustOK(t, l.Deposit(ctx, "d", ledger.Deposit{Key: "d", Amount: 10}))
	_, err = l.Hold(ctx, "d", ledger.Charge{Key: "h", Amount: 10})
	mustOK(t, err)
	now = now.Add(time.Hour)
	if _, err := l.ReleaseStale(cctx, time.Minute, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context stops the sweep, got %v", err)
	}
}

func TestReleaseGraceOption(t *testing.T) {
	s := ledger.NewMemoryStore()
	now := t0
	l := ledger.New(s, ledger.WithClock(func() time.Time { return now }), ledger.WithReleaseGrace(time.Hour))
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 30, Until: t0.Add(time.Minute)}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 30})
	mustOK(t, err)
	now = now.Add(2 * time.Minute)
	mustOK(t, l.Release(ctx, "a", h.ID))
	w, err := l.Balance(ctx, "a")
	mustOK(t, err)
	if w.Bonus != 30 || !w.BonusExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("WithReleaseGrace must set the grace, got %d until %s", w.Bonus, w.BonusExpiresAt)
	}
}

func TestMemoryStoreNegativeLimit(t *testing.T) {
	es, err := ledger.NewMemoryStore().Entries(ctx, "a", 0, -1)
	if err != nil || len(es) != 0 {
		t.Fatalf("a negative limit returns nothing, got %v, %v", es, err)
	}
}

func TestInsufficientErrorText(t *testing.T) {
	err := error(&ledger.InsufficientError{Required: 5, Available: 3})
	if err.Error() != "ledger: insufficient credits: need 5, have 3" {
		t.Fatalf("got %q", err.Error())
	}
}

var errDB = errors.New("db: down")

// brokenStore fails the call named in what, and serves the rest from memory.
type brokenStore struct {
	*ledger.MemoryStore
	what string
}

func (s *brokenStore) Wallet(c context.Context, account string) (ledger.Wallet, error) {
	if s.what == "wallet" {
		return ledger.Wallet{}, errDB
	}
	return s.MemoryStore.Wallet(c, account)
}

func (s *brokenStore) Entries(c context.Context, account string, before int64, limit int) ([]ledger.Entry, error) {
	if s.what == "entries" {
		return nil, errDB
	}
	return s.MemoryStore.Entries(c, account, before, limit)
}

func (s *brokenStore) StaleHolds(c context.Context, q ledger.StaleQuery) ([]ledger.Hold, error) {
	if s.what == "stale" {
		return nil, errDB
	}
	return s.MemoryStore.StaleHolds(c, q)
}

func (s *brokenStore) Update(c context.Context, account string, fn func(ledger.View) (*ledger.Change, error)) error {
	return s.MemoryStore.Update(c, account, func(v ledger.View) (*ledger.Change, error) {
		return fn(brokenView{View: v, what: s.what})
	})
}

type brokenView struct {
	ledger.View
	what string
}

func (v brokenView) Entry(c context.Context, key string) (ledger.Entry, bool, error) {
	if v.what == "entry" {
		return ledger.Entry{}, false, errDB
	}
	return v.View.Entry(c, key)
}

func (v brokenView) Hold(c context.Context, id string) (ledger.Hold, bool, error) {
	if v.what == "hold" {
		return ledger.Hold{}, false, errDB
	}
	return v.View.Hold(c, id)
}

func (v brokenView) Held(c context.Context) ([]ledger.Hold, error) {
	if v.what == "held" {
		return nil, errDB
	}
	return v.View.Held(c)
}

func TestStoreErrorsComeBackAsTheyAre(t *testing.T) {
	s := &brokenStore{MemoryStore: ledger.NewMemoryStore()}
	l := ledger.New(s, ledger.WithClock(func() time.Time { return t0 }))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 100})
	mustOK(t, err)

	calls := map[string]func() error{
		"wallet/balance": func() error { _, err := l.Balance(ctx, "a"); return err },
		"wallet/audit":   func() error { return l.Audit(ctx, "a") },
		"entries/audit":  func() error { return l.Audit(ctx, "a") },
		"stale/sweep":    func() error { _, err := l.ReleaseStale(ctx, time.Minute, 1); return err },
		"entry/grant":    func() error { return l.Grant(ctx, "a", ledger.Grant{Key: "g", Amount: 1, Until: month()}) },
		"entry/hold":     func() error { _, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 100}); return err },
		"entry/reverse":  func() error { _, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "r", Amount: 1}); return err },
		"entry/adjust":   func() error { return l.Adjust(ctx, "a", ledger.Adjustment{Key: "x", Purchased: 1}) },
		"hold/capture":   func() error { return l.Capture(ctx, "a", h.ID) },
		"held/reverse":   func() error { _, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "r", Amount: 1}); return err },
	}
	for name, call := range calls {
		s.what = strings.SplitN(name, "/", 2)[0]
		if err := call(); !errors.Is(err, errDB) {
			t.Errorf("%s: a store error must come back as it is, got %v", name, err)
		}
	}
	s.what = ""
	want(t, l, "a", 0, 0)

	if e := (ledger.Entry{Kind: ledger.KindDeposit, Purchased: 5}); e.Taken() != 0 || e.Short() != 0 {
		t.Fatal("Taken and Short are zero on every kind but a reversal")
	}
	if err := l.Spend(ctx, "a", ledger.Charge{Amount: 1}); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatal(err)
	}
	if err := l.Capture(ctx, "", h.ID); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := l.Balance(ctx, ""); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestHoldReplayOfMissingHoldIsCorrupt(t *testing.T) {
	l, s, _ := newLedger(t)
	err := s.Update(ctx, "a", func(ledger.View) (*ledger.Change, error) {
		return &ledger.Change{
			Wallet:  ledger.Wallet{Account: "a", Seq: 1},
			Entries: []ledger.Entry{{Account: "a", Seq: 1, Kind: ledger.KindHold, Key: "job", Hold: "gone"}},
		}, nil
	})
	mustOK(t, err)
	if _, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 1}); !errors.Is(err, ledger.ErrCorrupt) {
		t.Fatalf("a hold entry naming a hold that does not exist is corruption, got %v", err)
	}
	if _, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 1, Ref: "other"}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("got %v", err)
	}
}

func TestHoldRunsTheActionOnce(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))

	// Twenty workers pick up the same job at once: one gets the hold, the
	// others are told it exists, and the action runs once.
	var runs, exists atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Hold(ctx, "a", ledger.Charge{Key: "job", Amount: 40})
			switch {
			case err == nil:
				runs.Add(1)
			case errors.Is(err, ledger.ErrHoldExists):
				exists.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 1 || exists.Load() != 19 {
		t.Fatalf("the action must run once: %d runs, %d told the hold exists", runs.Load(), exists.Load())
	}
	want(t, l, "a", 0, 60)
}

func TestReverseReplayComparesExact(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 100}))
	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "r", Amount: 200})
	mustOK(t, err)
	if taken != 100 {
		t.Fatalf("want 100, got %d", taken)
	}
	if _, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "r", Amount: 200, Exact: true}); !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("a repeated reversal with another mode must be refused, not read as an exact one; got %v", err)
	}
	if e := entries(t, l, "a")[0]; e.Exact {
		t.Fatal("the entry records the mode of the first call")
	}
}

func TestReplaceEndsTheHeldBonus(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{Key: "p1", Amount: 100, Until: t0.Add(24 * time.Hour), Origin: ledger.OriginPlan}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "d", Amount: 10}))
	old, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 110}) // 100 bonus + 10 purchased
	mustOK(t, err)
	mustOK(t, l.Grant(ctx, "a", ledger.Grant{
		Key: "p2", Amount: 100, Until: t0.Add(48 * time.Hour), Replace: true, Origin: ledger.OriginPlan,
	}))
	fresh, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-2", Amount: 30})
	mustOK(t, err)

	mustOK(t, l.Release(ctx, "a", old.ID))
	want(t, l, "a", 70, 10) // the old period's 100 bonus are written off, the 10 purchased come back
	es := entries(t, l, "a")
	if es[0].Kind != ledger.KindExpire || es[0].Bonus != -100 || es[0].Hold != old.ID || es[1].Kind != ledger.KindRelease {
		t.Fatalf("a release after a replacement must record the write-off after the release, got %+v %+v", es[0], es[1])
	}

	mustOK(t, l.Release(ctx, "a", fresh.ID))
	want(t, l, "a", 100, 10) // a hold of the current period comes back whole
}

// failingAccounts fails every update of the named accounts.
type failingAccounts struct {
	*ledger.MemoryStore
	bad map[string]bool
}

func (s *failingAccounts) Update(c context.Context, account string, fn func(ledger.View) (*ledger.Change, error)) error {
	if s.bad[account] {
		return errors.New("db: row locked")
	}
	return s.MemoryStore.Update(c, account, fn)
}

func TestReleaseStaleWalksPastAFullPageOfFailures(t *testing.T) {
	s := &failingAccounts{MemoryStore: ledger.NewMemoryStore(), bad: map[string]bool{}}
	now := t0
	l := ledger.New(s, ledger.WithClock(func() time.Time { return now }))
	for _, acc := range []string{"bad-1", "bad-2", "bad-3", "good"} {
		mustOK(t, l.Deposit(ctx, acc, ledger.Deposit{Key: "d", Amount: 10}))
		_, err := l.Hold(ctx, acc, ledger.Charge{Key: "h", Amount: 10})
		mustOK(t, err)
		now = now.Add(time.Second)
	}
	now = now.Add(time.Hour)
	s.bad = map[string]bool{"bad-1": true, "bad-2": true, "bad-3": true}

	for _, limit := range []int{1, 3} {
		n, err := l.ReleaseStale(ctx, time.Minute, limit)
		if n != 1 || err == nil {
			t.Fatalf("limit %d: the oldest holds failing must not keep the sweep from the healthy one: "+
				"want 1 released and an error, got %d, %v", limit, n, err)
		}
		if strings.Count(err.Error(), "row locked") != 3 {
			t.Fatalf("every failure is reported, got %v", err)
		}
		want(t, l, "good", 0, 10)
		// Make the healthy hold stale again for the second limit.
		_, err = l.Hold(ctx, "good", ledger.Charge{Key: fmt.Sprint("h", limit), Amount: 10})
		mustOK(t, err)
		now = now.Add(time.Hour)
	}
}

// One refund applied by both paths - the withdrawal the application started
// and the provider's webhook - under the record's key is reversed once.
func TestOneRefundOneReversal(t *testing.T) {
	l, _, _ := newLedger(t)
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 100}))
	mustOK(t, l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_2", Amount: 100}))

	stored := ledger.Reversal{Key: "refund:r1", Amount: 100, Ref: "txn_1", Exact: true}
	for _, path := range []string{"withdrawal", "webhook", "webhook again"} {
		if _, err := l.Reverse(ctx, "a", stored); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	want(t, l, "a", 0, 100)
}
