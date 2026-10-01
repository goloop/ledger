// Package ledgertest checks a ledger.Store against the contract the ledger
// relies on.
//
// A store backed by a database is application code, and the ways it can go
// wrong are the ones a single sequential test does not show: a check and a
// write in two transactions, a rejected change that half-applies, two updates
// of one account that interleave. Store runs the whole contract, including
// the concurrency, and every failure names the rule that broke.
//
//	func TestPostgresStore(t *testing.T) {
//	    ledgertest.Store(t, func(t *testing.T) ledger.Store {
//	        return newPostgresStore(t, dsn) // empty, cleaned up through t
//	    })
//	}
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goloop/ledger"
)

// NewStore builds an empty store for one subtest and cleans it up through t.
// A fresh store per subtest keeps one check's rows out of another's.
type NewStore func(t *testing.T) ledger.Store

// Store runs the conformance suite against the stores newStore builds.
func Store(t *testing.T, newStore NewStore) {
	t.Helper()

	t.Run("an unknown account is an empty wallet", func(t *testing.T) {
		checkUnknown(t, newStore(t))
	})
	t.Run("a change is written whole", func(t *testing.T) {
		checkWrite(t, newStore(t))
	})
	t.Run("an error or a nil change writes nothing", func(t *testing.T) {
		checkNoWrite(t, newStore(t))
	})
	t.Run("a duplicate key rejects the whole change", func(t *testing.T) {
		checkDuplicate(t, newStore(t))
	})
	t.Run("accounts are separate", func(t *testing.T) {
		checkSeparate(t, newStore(t))
	})
	t.Run("entries page newest first", func(t *testing.T) {
		checkEntries(t, newStore(t))
	})
	t.Run("holds are replaced by id", func(t *testing.T) {
		checkHoldReplace(t, newStore(t))
	})
	t.Run("held lists the account's open holds, oldest first", func(t *testing.T) {
		checkHeld(t, newStore(t))
	})
	t.Run("stale holds", func(t *testing.T) {
		checkStale(t, newStore(t))
	})
	t.Run("updates of one account do not overlap", func(t *testing.T) {
		checkSerial(t, newStore(t))
	})
	t.Run("the ledger works on it", func(t *testing.T) {
		checkLedger(t, newStore(t))
	})
}

var ctx = context.Background()

// base is a fixed instant with microsecond precision, which is what most
// databases keep. The suite compares times truncated to the microsecond, so
// a store is not failed for a precision it was never asked for.
var base = time.Date(2030, 1, 2, 3, 4, 5, 123456000, time.UTC)

func sameTime(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

func entry(account string, seq int64, key string, purchased, after int64) ledger.Entry {
	return ledger.Entry{
		Account: account, Seq: seq, Kind: ledger.KindDeposit, Key: key,
		Purchased: purchased, PurchasedAfter: after, Ref: "ref-" + key, Note: "note", At: base,
	}
}

func wallet(account string, purchased, seq int64) ledger.Wallet {
	return ledger.Wallet{Account: account, Purchased: purchased, Seq: seq, UpdatedAt: base}
}

func put(t *testing.T, s ledger.Store, account string, ch ledger.Change) error {
	t.Helper()
	return s.Update(ctx, account, func(ledger.View) (*ledger.Change, error) { return &ch, nil })
}

func mustWallet(t *testing.T, s ledger.Store, account string) ledger.Wallet {
	t.Helper()
	w, err := s.Wallet(ctx, account)
	if err != nil {
		t.Fatalf("Wallet(%q): %v", account, err)
	}
	return w
}

func mustEntries(t *testing.T, s ledger.Store, account string, before int64, limit int) []ledger.Entry {
	t.Helper()
	es, err := s.Entries(ctx, account, before, limit)
	if err != nil {
		t.Fatalf("Entries(%q): %v", account, err)
	}
	return es
}

func checkUnknown(t *testing.T, s ledger.Store) {
	w := mustWallet(t, s, "nobody")
	if w.Account != "nobody" || w.Total() != 0 || w.Seq != 0 || !w.BonusExpiresAt.IsZero() {
		t.Fatalf("an account without entries must read as an empty wallet carrying its id, got %+v; "+
			"an error here would make every new user's first page fail", w)
	}
	if es := mustEntries(t, s, "nobody", 0, 10); len(es) != 0 {
		t.Fatalf("an unknown account has no entries, got %d", len(es))
	}
	err := s.Update(ctx, "nobody", func(v ledger.View) (*ledger.Change, error) {
		if w := v.Wallet(); w.Total() != 0 || w.Seq != 0 {
			return nil, fmt.Errorf("the locked wallet of a new account must be empty, got %+v", w)
		}
		if _, ok, err := v.Entry(ctx, "k"); ok || err != nil {
			return nil, fmt.Errorf("a new account has no entry, got ok=%v err=%v", ok, err)
		}
		if _, ok, err := v.Hold(ctx, "h"); ok || err != nil {
			return nil, fmt.Errorf("a new account has no hold, got ok=%v err=%v", ok, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkWrite(t *testing.T, s ledger.Store) {
	exp := base.Add(30 * 24 * time.Hour)
	w := ledger.Wallet{Account: "a", Bonus: 5, Purchased: 7, BonusExpiresAt: exp, Seq: 2, UpdatedAt: base}
	e1 := entry("a", 1, "k1", 7, 7)
	e2 := ledger.Entry{
		Account: "a", Seq: 2, Kind: ledger.KindGrant, Key: "k2", Bonus: 5,
		BonusAfter: 5, PurchasedAfter: 7, Origin: ledger.OriginPlan, Hold: "", Ref: "r", Note: "n", At: base,
	}
	e3 := ledger.Entry{
		Account: "a", Seq: 3, Kind: ledger.KindReverse, Key: "k4", Purchased: -1, Requested: 9, FromHolds: 4,
		BonusAfter: 5, PurchasedAfter: 6, At: base,
	}
	w.Purchased, w.Seq = 6, 3
	h := ledger.Hold{
		ID: "h1", Account: "a", Key: "k3", Amount: 3, FromBonus: 2, FromPurchased: 1, Reversed: 1,
		Ref: "feature", Status: ledger.HoldHeld, CreatedAt: base,
	}
	if err := put(t, s, "a", ledger.Change{Wallet: w, Entries: []ledger.Entry{e1, e2, e3}, Holds: []ledger.Hold{h}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := mustWallet(t, s, "a")
	if got.Bonus != 5 || got.Purchased != 6 || got.Seq != 3 || !sameTime(got.BonusExpiresAt, exp) || !sameTime(got.UpdatedAt, base) {
		t.Fatalf("the wallet must be stored as given, got %+v want %+v", got, w)
	}

	es := mustEntries(t, s, "a", 0, 10)
	if len(es) != 3 {
		t.Fatalf("every entry of the change must be stored, got %d", len(es))
	}
	if r := es[0]; r.Kind != ledger.KindReverse || r.Requested != 9 || r.FromHolds != 4 || r.Purchased != -1 {
		t.Fatalf("a reversal entry must keep Requested and FromHolds, got %+v want %+v", r, e3)
	}
	g := es[1]
	if g.Seq != 2 || g.Kind != ledger.KindGrant || g.Key != "k2" || g.Bonus != 5 || g.BonusAfter != 5 ||
		g.PurchasedAfter != 7 || g.Origin != ledger.OriginPlan || g.Ref != "r" || g.Note != "n" ||
		g.Account != "a" || !sameTime(g.At, base) {
		t.Fatalf("an entry must come back field for field, got %+v want %+v", g, e2)
	}

	err := s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		if lw := v.Wallet(); lw.Seq != 3 || lw.Purchased != 6 {
			return nil, fmt.Errorf("the locked wallet must be the stored one, got %+v", lw)
		}
		e, ok, err := v.Entry(ctx, "k1")
		if err != nil || !ok || e.Seq != 1 || e.Purchased != 7 {
			return nil, fmt.Errorf("View.Entry must find an entry by key, got %+v ok=%v err=%v", e, ok, err)
		}
		gh, ok, err := v.Hold(ctx, "h1")
		if err != nil || !ok || gh.Amount != 3 || gh.FromBonus != 2 || gh.FromPurchased != 1 || gh.Reversed != 1 ||
			gh.Status != ledger.HoldHeld || gh.Key != "k3" || gh.Ref != "feature" || !sameTime(gh.CreatedAt, base) ||
			!gh.SettledAt.IsZero() {
			return nil, fmt.Errorf("View.Hold must return the hold field for field, got %+v ok=%v err=%v", gh, ok, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var errStop = errors.New("stop")

func checkNoWrite(t *testing.T, s ledger.Store) {
	if err := put(t, s, "a", ledger.Change{Wallet: wallet("a", 1, 1), Entries: []ledger.Entry{entry("a", 1, "k1", 1, 1)}}); err != nil {
		t.Fatal(err)
	}
	err := s.Update(ctx, "a", func(ledger.View) (*ledger.Change, error) { return nil, errStop })
	if !errors.Is(err, errStop) {
		t.Fatalf("Update must return fn's error as it is, got %v", err)
	}
	err = s.Update(ctx, "a", func(ledger.View) (*ledger.Change, error) {
		// A change built and then abandoned: returning an error must drop it.
		_ = ledger.Change{Wallet: wallet("a", 99, 2), Entries: []ledger.Entry{entry("a", 2, "k2", 98, 99)}}
		return nil, errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("Update must return fn's error as it is, got %v", err)
	}
	if err := s.Update(ctx, "a", func(ledger.View) (*ledger.Change, error) { return nil, nil }); err != nil {
		t.Fatalf("a nil change is not an error, got %v", err)
	}
	if w := mustWallet(t, s, "a"); w.Purchased != 1 || w.Seq != 1 {
		t.Fatalf("an error or a nil change must leave the wallet alone, got %+v", w)
	}
	if es := mustEntries(t, s, "a", 0, 10); len(es) != 1 {
		t.Fatalf("an error or a nil change must write no entry, got %d entries", len(es))
	}
}

func checkDuplicate(t *testing.T, s ledger.Store) {
	if err := put(t, s, "a", ledger.Change{Wallet: wallet("a", 1, 1), Entries: []ledger.Entry{entry("a", 1, "k1", 1, 1)}}); err != nil {
		t.Fatal(err)
	}
	h := ledger.Hold{ID: "h-dup", Account: "a", Key: "x", Amount: 1, FromPurchased: 1, Status: ledger.HoldHeld, CreatedAt: base}
	err := put(t, s, "a", ledger.Change{
		Wallet:  wallet("a", 3, 3),
		Entries: []ledger.Entry{entry("a", 2, "k2", 1, 2), entry("a", 3, "k1", 1, 3)},
		Holds:   []ledger.Hold{h},
	})
	if !errors.Is(err, ledger.ErrDuplicateKey) {
		t.Fatalf("an entry with a key the account already has must fail with ErrDuplicateKey, got %v; "+
			"this is the last line of defence against a double credit", err)
	}
	if w := mustWallet(t, s, "a"); w.Purchased != 1 || w.Seq != 1 {
		t.Fatalf("a rejected change must not move the wallet, got %+v", w)
	}
	if es := mustEntries(t, s, "a", 0, 10); len(es) != 1 {
		t.Fatalf("a rejected change must not append any of its entries, got %d entries", len(es))
	}
	_ = s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		if _, ok, _ := v.Hold(ctx, "h-dup"); ok {
			t.Errorf("a rejected change must not store its holds")
		}
		return nil, nil
	})

	err = put(t, s, "a", ledger.Change{
		Wallet:  wallet("a", 3, 3),
		Entries: []ledger.Entry{entry("a", 2, "same", 1, 2), entry("a", 3, "same", 1, 3)},
	})
	if !errors.Is(err, ledger.ErrDuplicateKey) {
		t.Fatalf("two entries with one key inside a change must fail with ErrDuplicateKey, got %v", err)
	}
}

func checkSeparate(t *testing.T, s ledger.Store) {
	for _, acc := range []string{"a", "b"} {
		err := put(t, s, acc, ledger.Change{
			Wallet:  wallet(acc, 1, 1),
			Entries: []ledger.Entry{entry(acc, 1, "shared-key", 1, 1)},
			Holds:   []ledger.Hold{{ID: "h-" + acc, Account: acc, Key: "x", Amount: 1, FromPurchased: 1, Status: ledger.HoldHeld, CreatedAt: base}},
		})
		if err != nil {
			t.Fatalf("keys are unique per account, so two accounts may use the same one; got %v for %q", err, acc)
		}
	}
	_ = s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		if _, ok, _ := v.Hold(ctx, "h-b"); ok {
			t.Errorf("View.Hold must not see another account's hold")
		}
		return nil, nil
	})
	if es := mustEntries(t, s, "a", 0, 10); len(es) != 1 || es[0].Account != "a" {
		t.Fatalf("Entries must return only the account's own entries, got %+v", es)
	}
}

func checkEntries(t *testing.T, s ledger.Store) {
	const n = 7
	for i := int64(1); i <= n; i++ {
		err := put(t, s, "a", ledger.Change{
			Wallet:  wallet("a", i, i),
			Entries: []ledger.Entry{entry("a", i, fmt.Sprint("k", i), 1, i)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first := mustEntries(t, s, "a", 0, 3)
	if len(first) != 3 || first[0].Seq != 7 || first[1].Seq != 6 || first[2].Seq != 5 {
		t.Fatalf("Entries(before=0, limit=3) must return 7, 6, 5, got %v", seqs(first))
	}
	next := mustEntries(t, s, "a", first[2].Seq, 3)
	if len(next) != 3 || next[0].Seq != 4 || next[2].Seq != 2 {
		t.Fatalf("Entries(before=5, limit=3) must return 4, 3, 2, got %v", seqs(next))
	}
	last := mustEntries(t, s, "a", next[2].Seq, 3)
	if len(last) != 1 || last[0].Seq != 1 {
		t.Fatalf("Entries(before=2, limit=3) must return 1, got %v", seqs(last))
	}
}

func seqs(es []ledger.Entry) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}

func checkHoldReplace(t *testing.T, s ledger.Store) {
	h := ledger.Hold{ID: "h1", Account: "a", Key: "k", Amount: 4, FromPurchased: 4, Status: ledger.HoldHeld, CreatedAt: base}
	if err := put(t, s, "a", ledger.Change{Wallet: wallet("a", 0, 1), Entries: []ledger.Entry{entry("a", 1, "k", 0, 0)}, Holds: []ledger.Hold{h}}); err != nil {
		t.Fatal(err)
	}
	h.Status = ledger.HoldCaptured
	h.SettledAt = base.Add(time.Minute)
	if err := put(t, s, "a", ledger.Change{Wallet: wallet("a", 0, 2), Entries: []ledger.Entry{entry("a", 2, "c", 0, 0)}, Holds: []ledger.Hold{h}}); err != nil {
		t.Fatalf("a hold with an existing id must replace it, got %v", err)
	}
	_ = s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		got, ok, err := v.Hold(ctx, "h1")
		if err != nil || !ok || got.Status != ledger.HoldCaptured || !sameTime(got.SettledAt, h.SettledAt) {
			t.Errorf("the replaced hold must be the new one, got %+v ok=%v err=%v", got, ok, err)
		}
		return nil, nil
	})
}

func checkHeld(t *testing.T, s ledger.Store) {
	mk := func(acc, id string, age time.Duration, st ledger.HoldStatus) ledger.Hold {
		return ledger.Hold{ID: id, Account: acc, Key: id, Amount: 2, FromPurchased: 2, Status: st, CreatedAt: base.Add(-age)}
	}
	seq := map[string]int64{}
	add := func(h ledger.Hold) {
		seq[h.Account]++
		n := seq[h.Account]
		if err := put(t, s, h.Account, ledger.Change{
			Wallet: wallet(h.Account, 0, n), Entries: []ledger.Entry{entry(h.Account, n, fmt.Sprint(h.ID, "-", n), 0, 0)}, Holds: []ledger.Hold{h},
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(mk("a", "new", time.Minute, ledger.HoldHeld))
	add(mk("a", "old", time.Hour, ledger.HoldHeld))
	add(mk("a", "done", 2*time.Hour, ledger.HoldCaptured))
	add(mk("b", "other", 3*time.Hour, ledger.HoldHeld))
	old := mk("a", "old", time.Hour, ledger.HoldHeld)
	old.Reversed = 2
	add(old)

	err := s.Update(ctx, "a", func(v ledger.View) (*ledger.Change, error) {
		hs, err := v.Held(ctx)
		if err != nil {
			return nil, err
		}
		if len(hs) != 2 || hs[0].ID != "old" || hs[1].ID != "new" {
			return nil, fmt.Errorf("View.Held must return the account's held holds, oldest first: want [old new], got %v; "+
				"a reversal reads it to take back credits in flight", holdIDs(hs))
		}
		if hs[0].Reversed != 2 || hs[0].Account != "a" {
			return nil, fmt.Errorf("View.Held must return holds field for field, as last replaced; got %+v", hs[0])
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkStale(t *testing.T, s ledger.Store) {
	mk := func(acc, id string, age time.Duration, st ledger.HoldStatus) ledger.Hold {
		return ledger.Hold{ID: id, Account: acc, Key: id, Amount: 1, FromPurchased: 1, Status: st, CreatedAt: base.Add(-age)}
	}
	seq := map[string]int64{}
	add := func(h ledger.Hold) {
		seq[h.Account]++
		n := seq[h.Account]
		if err := put(t, s, h.Account, ledger.Change{
			Wallet: wallet(h.Account, 0, n), Entries: []ledger.Entry{entry(h.Account, n, h.ID, 0, 0)}, Holds: []ledger.Hold{h},
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(mk("a", "old", 3*time.Hour, ledger.HoldHeld))
	add(mk("b", "older", 4*time.Hour, ledger.HoldHeld))
	add(mk("a", "fresh", time.Minute, ledger.HoldHeld))
	add(mk("b", "settled", 5*time.Hour, ledger.HoldCaptured))
	add(mk("a", "released", 5*time.Hour, ledger.HoldReleased))

	got, err := s.StaleHolds(ctx, base.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "older" || got[1].ID != "old" {
		t.Fatalf("StaleHolds must return the held holds older than the limit, of every account, oldest first: "+
			"want [older old], got %v", holdIDs(got))
	}
	if got[0].Account != "b" {
		t.Fatalf("a stale hold must carry its account, or it cannot be released; got %q", got[0].Account)
	}
	if one, _ := s.StaleHolds(ctx, base.Add(-time.Hour), 1); len(one) != 1 || one[0].ID != "older" {
		t.Fatalf("StaleHolds must honour the limit and keep the oldest, got %v", holdIDs(one))
	}
}

func holdIDs(hs []ledger.Hold) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.ID
	}
	return out
}

func checkSerial(t *testing.T, s ledger.Store) {
	l := ledger.New(s)
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				key := fmt.Sprintf("w%d-%d", w, i)
				if err := l.Deposit(ctx, "a", ledger.Deposit{Key: key, Amount: 1}); err != nil {
					errs <- err
					return
				}
				if err := l.Deposit(ctx, fmt.Sprint("p", w), ledger.Deposit{Key: key, Amount: 1}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent deposits must all succeed on a correct store, got %v", err)
	}
	w := mustWallet(t, s, "a")
	if w.Purchased != workers*each || w.Seq != workers*each {
		t.Fatalf("%d concurrent deposits of 1 must end at %d with Seq %d, got %d with Seq %d: "+
			"two updates of one account overlapped and one overwrote the other",
			workers*each, workers*each, workers*each, w.Purchased, w.Seq)
	}
	if err := l.Audit(ctx, "a"); err != nil {
		t.Fatalf("the history after concurrent deposits must add up: %v", err)
	}
}

func checkLedger(t *testing.T, s ledger.Store) {
	now := base
	l := ledger.New(s, ledger.WithClock(func() time.Time { return now }))

	if err := l.Grant(ctx, "a", ledger.Grant{Key: "welcome", Amount: 300, Until: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 1000, Ref: "txn_1"}); err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if err := l.Deposit(ctx, "a", ledger.Deposit{Key: "txn_1", Amount: 1000, Ref: "txn_1"}); err != nil {
		t.Fatalf("a repeated Deposit must be a no-op, got %v", err)
	}
	h, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-1", Amount: 500, Ref: "convert"})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := l.Capture(ctx, "a", h.ID); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	h2, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-2", Amount: 100, Ref: "convert"})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := l.Release(ctx, "a", h2.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	now = now.Add(31 * 24 * time.Hour)
	if err := l.Spend(ctx, "a", ledger.Charge{Key: "job-3", Amount: 100}); err != nil {
		t.Fatalf("Spend: %v", err)
	}
	h4, err := l.Hold(ctx, "a", ledger.Charge{Key: "job-4", Amount: 650})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	taken, err := l.Reverse(ctx, "a", ledger.Reversal{Key: "adj_1", Amount: 200, Ref: "txn_1"})
	if err != nil || taken != 200 {
		t.Fatalf("a reversal must reach the credits in a hold: want 200, got %d, %v", taken, err)
	}
	if err := l.Release(ctx, "a", h4.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	w, err := l.Balance(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// 300 bonus + 1000 purchased; 500 captured (300 bonus, 200 purchased);
	// 100 held and released; the empty bonus expires; 100 spent; 650 held,
	// 200 of them reversed, 450 released.
	if w.Bonus != 0 || w.Purchased != 500 {
		t.Fatalf("after the scenario the wallet must hold 0/500, got %d/%d", w.Bonus, w.Purchased)
	}
	if err := l.Audit(ctx, "a"); err != nil {
		t.Fatalf("the history must add up: %v", err)
	}
	if n, err := l.ReleaseStale(ctx, time.Hour, 10); err != nil || n != 0 {
		t.Fatalf("no hold is left open, so ReleaseStale must release none; got %d, %v", n, err)
	}
}
