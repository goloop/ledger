package ledger

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultReleaseGrace is how long bonus credits brought back by a released
// hold live when the bonus bucket expired while the action ran.
const DefaultReleaseGrace = 24 * time.Hour

// MaxKeyLength is the longest key the ledger accepts, in bytes.
const MaxKeyLength = 255

// MaxAmount is the largest amount a movement may carry and a bucket may hold:
// 2^53 - 1, the largest integer every JSON reader keeps exact. It is far
// beyond any real balance, so reaching it means an amount came from somewhere
// it should not have, and the ledger refuses it instead of overflowing.
const MaxAmount = 1<<53 - 1

// reservedPrefix starts the keys the ledger makes for its own entries
// (captures, releases, expiries). Callers cannot use it, so a caller key can
// never collide with one of them.
const reservedPrefix = "ledger:"

// Entries page sizes.
const (
	defaultPage = 50
	maxPage     = 1000
)

// Ledger applies movements to wallets kept in a Store.
//
// Every method that changes a wallet takes a key, and a repeated call with
// the same key changes nothing: a webhook delivered twice, a request retried
// after a timeout, a worker restarted halfway through a batch all land on the
// entry the first call made. That is the property a payment flow needs most
// and gets wrong most often, so it is not optional here.
//
// A Ledger is safe for concurrent use; the store serializes updates of one
// account.
type Ledger struct {
	store Store
	now   func() time.Time
	grace time.Duration
}

// Option configures a Ledger in New.
type Option func(*Ledger)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) { l.now = now }
}

// WithReleaseGrace sets how long bonus credits brought back by a released
// hold live when the bucket expired while the action ran. The default is
// DefaultReleaseGrace.
//
// Without a grace the credits would come back already expired: a failed
// action would have cost the user the chance to spend them, and a failure is
// supposed to cost nothing.
func WithReleaseGrace(d time.Duration) Option {
	return func(l *Ledger) { l.grace = d }
}

// New returns a Ledger over the store.
func New(store Store, opts ...Option) *Ledger {
	l := &Ledger{store: store, now: time.Now, grace: DefaultReleaseGrace}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Grant adds bonus credits that expire.
type Grant struct {
	// Key makes the grant idempotent. A monthly allowance keyed by its
	// period ("allowance:2026-09") is granted once however many times the
	// code that grants it runs.
	Key string

	// Amount is how much to add; it must be positive.
	Amount int64

	// Until is when the credits expire; it must be in the future. The
	// bucket's expiry moves to Until if that is later than the current one,
	// and never earlier.
	Until time.Time

	// Replace expires whatever bonus is left before adding this grant, and
	// sets the expiry to Until even if the current one is later. It is how
	// a new plan period replaces the previous period's allowance instead of
	// piling on top of it.
	Replace bool

	// Origin records why the credits were granted; empty means OriginPromo.
	Origin Origin

	// Ref and Note are stored on the entry and not interpreted.
	Ref  string
	Note string
}

// Deposit adds purchased credits, which never expire.
type Deposit struct {
	// Key makes the deposit idempotent. The provider's transaction id is
	// the natural choice: one payment, one deposit, however many times the
	// webhook arrives.
	Key string

	// Amount is how much to add; it must be positive.
	Amount int64

	Ref  string
	Note string
}

// Charge is the price of an action, for Hold and Spend.
type Charge struct {
	// Key makes the charge idempotent: the id of the job, the request, the
	// order the action belongs to.
	Key string

	// Amount is the price; it must be positive. A free action does not
	// touch the ledger at all.
	Amount int64

	// Ref is usually the feature being paid for.
	Ref  string
	Note string
}

// Reversal takes purchased credits back after the money went back: a refund
// or a chargeback.
type Reversal struct {
	// Key makes the reversal idempotent: the provider's refund or
	// adjustment id, never the transaction id, because one transaction can
	// be refunded in parts.
	Key string

	// Amount is how much the reversed money bought; it must be positive.
	Amount int64

	// Exact takes the whole amount from the purchased bucket or nothing: a
	// wallet that holds less fails with an *InsufficientError and nothing is
	// written. It is the check for a withdrawal of an unused pack, made in
	// the same step as the reversal so nothing can be spent in between.
	// Credits in holds do not count: an action in flight is use.
	Exact bool

	Ref  string
	Note string
}

// Adjustment is a manual correction. Positive amounts add, negative ones
// take away; the buckets cannot go below zero.
type Adjustment struct {
	Key string

	// Bonus and Purchased are the changes; at least one must be non-zero.
	Bonus     int64
	Purchased int64

	// BonusUntil is the expiry for bonus credits added to a bucket that has
	// none; it extends the current expiry the way Grant.Until does. It is
	// required when Bonus is positive and the bucket has no expiry.
	BonusUntil time.Time

	Ref  string
	Note string
}

// Balance returns the account's wallet as of now, with a bonus bucket past
// its expiry shown as empty.
//
// It only reads. The expiry is written to the ledger by the next movement
// of the account, which is also when it starts to matter; a balance page
// read a thousand times a day does not need to take a lock a thousand times.
func (l *Ledger) Balance(ctx context.Context, account string) (Wallet, error) {
	if account == "" {
		return Wallet{}, invalid("empty account")
	}
	w, err := l.store.Wallet(ctx, account)
	if err != nil {
		return Wallet{}, err
	}
	w.Account = account
	if exp := w.BonusExpiresAt; !exp.IsZero() && !exp.After(l.now()) {
		w.Bonus = 0
		w.BonusExpiresAt = time.Time{}
	}
	return w, nil
}

// Grant adds bonus credits. A repeated call with the same key changes
// nothing.
func (l *Ledger) Grant(ctx context.Context, account string, g Grant) error {
	if err := checkKey(g.Key); err != nil {
		return err
	}
	if err := checkAmount("grant", g.Amount); err != nil {
		return err
	}
	if g.Until.IsZero() {
		return invalid("grant needs an expiry")
	}
	switch g.Origin {
	case "":
		g.Origin = OriginPromo
	case OriginPromo, OriginPlan:
	default:
		return invalid("unknown origin %q", g.Origin)
	}

	return l.update(ctx, account, func(u *unit) error {
		done, err := u.replay(g.Key, func(e Entry) bool {
			return e.Kind == KindGrant && e.Bonus == g.Amount && e.Origin == g.Origin && e.Ref == g.Ref
		})
		if done || err != nil {
			return err
		}
		if !g.Until.After(u.now) {
			return invalid("grant expiry %s is not in the future", g.Until.Format(time.RFC3339))
		}
		if g.Replace {
			if u.w.Bonus > 0 {
				err := u.move(Entry{
					Kind:  KindExpire,
					Key:   u.ownKey("replace"),
					Bonus: -u.w.Bonus,
					Note:  "replaced by a new grant",
				})
				if err != nil {
					return err
				}
			}
			u.w.BonusExpiresAt = g.Until
		} else {
			u.extend(g.Until)
		}
		return u.move(Entry{
			Kind:   KindGrant,
			Key:    g.Key,
			Bonus:  g.Amount,
			Origin: g.Origin,
			Ref:    g.Ref,
			Note:   g.Note,
		})
	})
}

// Deposit adds purchased credits. A repeated call with the same key changes
// nothing.
func (l *Ledger) Deposit(ctx context.Context, account string, d Deposit) error {
	if err := checkKey(d.Key); err != nil {
		return err
	}
	if err := checkAmount("deposit", d.Amount); err != nil {
		return err
	}
	return l.update(ctx, account, func(u *unit) error {
		done, err := u.replay(d.Key, func(e Entry) bool {
			return e.Kind == KindDeposit && e.Purchased == d.Amount && e.Ref == d.Ref
		})
		if done || err != nil {
			return err
		}
		return u.move(Entry{
			Kind:      KindDeposit,
			Key:       d.Key,
			Purchased: d.Amount,
			Ref:       d.Ref,
			Note:      d.Note,
		})
	})
}

// Hold sets the price of an action aside, bonus credits first, and returns
// the hold. The caller captures it when the action succeeds and releases it
// when the action fails.
//
// A repeated call with the same key takes nothing more and returns the hold
// the first call made. A hold that was released comes back with ErrReleased:
// the action it paid for must not run on it, and a new attempt needs a new
// key. A captured one comes back without an error and with Status
// HoldCaptured, which means the action was already paid for and ran. A wallet
// that cannot cover the amount returns an *InsufficientError.
func (l *Ledger) Hold(ctx context.Context, account string, c Charge) (Hold, error) {
	if err := checkCharge(c); err != nil {
		return Hold{}, err
	}
	var out Hold
	err := l.update(ctx, account, func(u *unit) error {
		e, ok, err := u.v.Entry(u.ctx, c.Key)
		if err != nil {
			return err
		}
		if ok {
			if e.Kind != KindHold || e.Ref != c.Ref {
				return fmt.Errorf("%w: %q", ErrKeyReused, c.Key)
			}
			h, found, err := u.v.Hold(u.ctx, e.Hold)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: entry %d names hold %q, which does not exist", ErrCorrupt, e.Seq, e.Hold)
			}
			if h.Amount != c.Amount {
				return fmt.Errorf("%w: %q", ErrKeyReused, c.Key)
			}
			out = h
			if h.Status == HoldReleased {
				return fmt.Errorf("%w: %q was released, the key %q needs a new attempt", ErrReleased, h.ID, c.Key)
			}
			return nil
		}

		if have := u.w.Total(); have < c.Amount {
			return &InsufficientError{Required: c.Amount, Available: have}
		}
		fromBonus := min(u.w.Bonus, c.Amount)
		h := Hold{
			ID:            rand.Text(),
			Account:       u.w.Account,
			Key:           c.Key,
			Amount:        c.Amount,
			FromBonus:     fromBonus,
			FromPurchased: c.Amount - fromBonus,
			Ref:           c.Ref,
			Status:        HoldHeld,
			CreatedAt:     u.now,
		}
		err = u.move(Entry{
			Kind:      KindHold,
			Key:       c.Key,
			Bonus:     -h.FromBonus,
			Purchased: -h.FromPurchased,
			Hold:      h.ID,
			Ref:       c.Ref,
			Note:      c.Note,
		})
		if err != nil {
			return err
		}
		u.holds = append(u.holds, h)
		out = h
		return nil
	})
	if errors.Is(err, ErrReleased) {
		return out, err
	}
	if err != nil {
		return Hold{}, err
	}
	return out, nil
}

// Capture settles a hold as spent.
//
// Capturing a captured hold is a no-op. Capturing a released hold returns
// ErrReleased: the credits are already back in the wallet, and pretending
// otherwise would let an action run for free without anyone noticing.
func (l *Ledger) Capture(ctx context.Context, account, holdID string) error {
	_, err := l.settle(ctx, account, holdID, HoldCaptured)
	return err
}

// Release returns a hold to the buckets it came from.
//
// Releasing a released hold is a no-op, and so is releasing a captured one:
// a deferred Release after a successful Capture is the natural way to write
// "release unless it was captured", and it should not need a condition.
func (l *Ledger) Release(ctx context.Context, account, holdID string) error {
	_, err := l.settle(ctx, account, holdID, HoldReleased)
	return err
}

// Spend charges an action at once, bonus credits first: a hold and a capture
// in one entry, for an action that cannot fail after it is paid for. A
// repeated call with the same key changes nothing.
func (l *Ledger) Spend(ctx context.Context, account string, c Charge) error {
	if err := checkCharge(c); err != nil {
		return err
	}
	return l.update(ctx, account, func(u *unit) error {
		done, err := u.replay(c.Key, func(e Entry) bool {
			return e.Kind == KindSpend && -(e.Bonus+e.Purchased) == c.Amount && e.Ref == c.Ref
		})
		if done || err != nil {
			return err
		}
		if have := u.w.Total(); have < c.Amount {
			return &InsufficientError{Required: c.Amount, Available: have}
		}
		fromBonus := min(u.w.Bonus, c.Amount)
		return u.move(Entry{
			Kind:      KindSpend,
			Key:       c.Key,
			Bonus:     -fromBonus,
			Purchased: -(c.Amount - fromBonus),
			Ref:       c.Ref,
			Note:      c.Note,
		})
	})
}

// Reverse takes purchased credits back and returns how many it took.
//
// It takes what is left in the purchased bucket first, then the purchased
// share of holds still in flight: the money for those credits went back to
// the customer, so a release must not return them. Each hold it reaches
// records the part taken in Hold.Reversed. Credits already spent stay spent,
// because the action they paid for already ran; the difference between the
// amount and what was taken is the shortfall the business absorbs, recorded
// on the entry (Entry.Short). Bonus credits are never taken: they were not
// paid for, so a refund does not reach them.
//
// With Exact, the purchased bucket alone must cover the amount, or nothing
// is taken and an *InsufficientError is returned.
//
// A repeated call with the same key takes nothing and returns what the first
// call took.
func (l *Ledger) Reverse(ctx context.Context, account string, r Reversal) (int64, error) {
	if err := checkKey(r.Key); err != nil {
		return 0, err
	}
	if err := checkAmount("reversal", r.Amount); err != nil {
		return 0, err
	}
	var taken int64
	err := l.update(ctx, account, func(u *unit) error {
		e, ok, err := u.v.Entry(u.ctx, r.Key)
		if err != nil {
			return err
		}
		if ok {
			if e.Kind != KindReverse || e.Requested != r.Amount || e.Ref != r.Ref {
				return fmt.Errorf("%w: %q", ErrKeyReused, r.Key)
			}
			taken = e.Taken()
			return nil
		}

		fromWallet := min(u.w.Purchased, r.Amount)
		if r.Exact && fromWallet < r.Amount {
			return &InsufficientError{Required: r.Amount, Available: u.w.Purchased}
		}
		var fromHolds int64
		if fromWallet < r.Amount {
			held, err := u.v.Held(u.ctx)
			if err != nil {
				return err
			}
			for _, h := range held {
				n := min(r.Amount-fromWallet-fromHolds, h.FromPurchased-h.Reversed)
				if n <= 0 {
					continue
				}
				h.Reversed += n
				fromHolds += n
				u.holds = append(u.holds, h)
				if fromWallet+fromHolds == r.Amount {
					break
				}
			}
		}
		taken = fromWallet + fromHolds
		return u.move(Entry{
			Kind:      KindReverse,
			Key:       r.Key,
			Purchased: -fromWallet,
			Requested: r.Amount,
			FromHolds: fromHolds,
			Ref:       r.Ref,
			Note:      r.Note,
		})
	})
	if err != nil {
		return 0, err
	}
	return taken, nil
}

// Adjust applies a manual correction. A repeated call with the same key
// changes nothing. A correction that would take a bucket below zero returns
// an *InsufficientError.
func (l *Ledger) Adjust(ctx context.Context, account string, a Adjustment) error {
	if err := checkKey(a.Key); err != nil {
		return err
	}
	if a.Bonus == 0 && a.Purchased == 0 {
		return invalid("adjustment changes nothing")
	}
	if a.Bonus < -MaxAmount || a.Bonus > MaxAmount || a.Purchased < -MaxAmount || a.Purchased > MaxAmount {
		return invalid("adjustment beyond MaxAmount")
	}
	return l.update(ctx, account, func(u *unit) error {
		done, err := u.replay(a.Key, func(e Entry) bool {
			return e.Kind == KindAdjust && e.Bonus == a.Bonus && e.Purchased == a.Purchased && e.Ref == a.Ref
		})
		if done || err != nil {
			return err
		}
		if a.Bonus < 0 && u.w.Bonus < -a.Bonus {
			return &InsufficientError{Required: -a.Bonus, Available: u.w.Bonus}
		}
		if a.Purchased < 0 && u.w.Purchased < -a.Purchased {
			return &InsufficientError{Required: -a.Purchased, Available: u.w.Purchased}
		}
		if !a.BonusUntil.IsZero() {
			if !a.BonusUntil.After(u.now) {
				return invalid("bonus expiry %s is not in the future", a.BonusUntil.Format(time.RFC3339))
			}
			if a.Bonus > 0 {
				u.extend(a.BonusUntil)
			}
		}
		if a.Bonus > 0 && u.w.BonusExpiresAt.IsZero() {
			return invalid("bonus credits need an expiry: set BonusUntil")
		}
		return u.move(Entry{
			Kind:      KindAdjust,
			Key:       a.Key,
			Bonus:     a.Bonus,
			Purchased: a.Purchased,
			Ref:       a.Ref,
			Note:      a.Note,
		})
	})
}

// ReleaseStale releases up to limit holds that have been held longer than
// maxAge - left behind by a process that died between the hold and its
// settlement - and returns how many it released.
//
// A hold that fails to release does not stop the others: the sweep goes on,
// and the failures come back joined in the error, next to the count of the
// holds it did release. Only a cancelled context stops it early.
//
// Run it from a periodic worker, with maxAge comfortably longer than the
// slowest action and than the time a failed Capture is retried: an action
// that finishes after its hold was released gets ErrReleased from Capture,
// and has run unpaid.
func (l *Ledger) ReleaseStale(ctx context.Context, maxAge time.Duration, limit int) (int, error) {
	if maxAge <= 0 {
		return 0, invalid("max age must be positive")
	}
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	holds, err := l.store.StaleHolds(ctx, l.now().Add(-maxAge), limit)
	if err != nil {
		return 0, err
	}
	released := 0
	var errs []error
	for _, h := range holds {
		if err := ctx.Err(); err != nil {
			return released, errors.Join(append(errs, err)...)
		}
		changed, err := l.settle(ctx, h.Account, h.ID, HoldReleased)
		if err != nil {
			errs = append(errs, fmt.Errorf("hold %q of %q: %w", h.ID, h.Account, err))
			continue
		}
		if changed {
			released++
		}
	}
	return released, errors.Join(errs...)
}

// Entries returns up to limit entries of the account, newest first, whose
// Seq is below before; before <= 0 starts from the newest. Pass the Seq of
// the last entry of one page as before to get the next.
func (l *Ledger) Entries(ctx context.Context, account string, before int64, limit int) ([]Entry, error) {
	if account == "" {
		return nil, invalid("empty account")
	}
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	return l.store.Entries(ctx, account, before, limit)
}

// Audit replays the account's whole history and checks that it adds up:
// entries numbered without gaps, every recorded balance equal to the one
// before it plus the movement, no balance below zero, and the last one equal
// to the wallet. A discrepancy is returned wrapped in ErrCorrupt.
//
// It takes no lock. The wallet is read first, and only the entries up to its
// Seq are checked: the history is append-only, so those never change, and a
// movement made while the audit runs cannot make a healthy account look
// broken. It walks the history from the newest entry back, a page at a time,
// so a long history is not held in memory.
//
// It reads every entry of the account, so it belongs in a reconciliation job
// or an operator's tool, not in a request.
func (l *Ledger) Audit(ctx context.Context, account string) error {
	if account == "" {
		return invalid("empty account")
	}
	w, err := l.store.Wallet(ctx, account)
	if err != nil {
		return err
	}

	// Walking back, each entry must record the balances the newer entries
	// leave, and taking its movement away must give the balances of the
	// entry before it. The history starts from an empty wallet at Seq 0.
	seq, bonus, purchased := w.Seq, w.Bonus, w.Purchased
	before := w.Seq + 1
	for seq > 0 {
		page, err := l.store.Entries(ctx, account, before, maxPage)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if e.Seq != seq {
				return fmt.Errorf("%w: expected entry %d, found %d", ErrCorrupt, seq, e.Seq)
			}
			if e.BonusAfter != bonus || e.PurchasedAfter != purchased {
				return fmt.Errorf("%w: entry %d records %d/%d, the wallet and the entries after it say %d/%d",
					ErrCorrupt, e.Seq, e.BonusAfter, e.PurchasedAfter, bonus, purchased)
			}
			if bonus < 0 || purchased < 0 {
				return fmt.Errorf("%w: entry %d leaves a bucket below zero", ErrCorrupt, e.Seq)
			}
			bonus -= e.Bonus
			purchased -= e.Purchased
			seq--
			if seq == 0 {
				break
			}
		}
		before = page[len(page)-1].Seq
	}
	if seq != 0 {
		return fmt.Errorf("%w: the wallet is at entry %d, entries 1 to %d are missing", ErrCorrupt, w.Seq, seq)
	}
	if bonus != 0 || purchased != 0 {
		return fmt.Errorf("%w: the history does not start from an empty wallet: %d/%d before entry 1",
			ErrCorrupt, bonus, purchased)
	}

	// An entry newer than the wallet is either a movement made during the
	// audit or a wallet that missed its update. The store writes both in one
	// change, so a wallet read after the entry was seen must have caught up.
	newest, err := l.store.Entries(ctx, account, 0, 1)
	if err != nil {
		return err
	}
	if len(newest) == 1 && newest[0].Seq > w.Seq {
		again, err := l.store.Wallet(ctx, account)
		if err != nil {
			return err
		}
		if again.Seq < newest[0].Seq {
			return fmt.Errorf("%w: entry %d exists, the wallet is at entry %d", ErrCorrupt, newest[0].Seq, again.Seq)
		}
	}
	return nil
}

// settle captures or releases a hold and reports whether it changed.
func (l *Ledger) settle(ctx context.Context, account, holdID string, to HoldStatus) (bool, error) {
	if holdID == "" {
		return false, invalid("empty hold id")
	}
	changed := false
	err := l.update(ctx, account, func(u *unit) error {
		changed = false
		h, ok, err := u.v.Hold(u.ctx, holdID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %q", ErrNotFound, holdID)
		}
		switch h.Status {
		case HoldCaptured:
			return nil
		case HoldReleased:
			if to == HoldCaptured {
				return fmt.Errorf("%w: %q", ErrReleased, holdID)
			}
			return nil
		}

		h.Status = to
		h.SettledAt = u.now
		en := Entry{Kind: KindCapture, Key: reservedPrefix + "capture:" + h.ID, Hold: h.ID, Ref: h.Ref}
		if to == HoldReleased {
			en.Kind = KindRelease
			en.Key = reservedPrefix + "release:" + h.ID
			en.Bonus, en.Purchased = h.FromBonus, h.FromPurchased-h.Reversed
			if h.FromBonus > 0 && u.w.BonusExpiresAt.IsZero() {
				u.w.BonusExpiresAt = u.now.Add(l.grace)
			}
		}
		if err := u.move(en); err != nil {
			return err
		}
		u.holds = append(u.holds, h)
		changed = true
		return nil
	})
	return changed, err
}

// update runs fn on the locked wallet, after writing off an expired bonus.
func (l *Ledger) update(ctx context.Context, account string, fn func(*unit) error) error {
	if account == "" {
		return invalid("empty account")
	}
	return l.store.Update(ctx, account, func(v View) (*Change, error) {
		u := &unit{ctx: ctx, v: v, w: v.Wallet(), now: l.now().UTC()}
		u.w.Account = account
		if err := u.expire(); err != nil {
			return nil, err
		}
		if err := fn(u); err != nil {
			return nil, err
		}
		if !u.dirty {
			return nil, nil
		}
		u.w.UpdatedAt = u.now
		return &Change{Wallet: u.w, Entries: u.entries, Holds: u.holds}, nil
	})
}

// unit is one Store.Update in progress: the locked wallet and what is to be
// written.
type unit struct {
	ctx     context.Context
	v       View
	w       Wallet
	now     time.Time
	entries []Entry
	holds   []Hold
	dirty   bool
}

// move applies an entry's changes to the wallet and queues the entry. The
// buckets are checked here as well as by every caller: the callers check to
// return a useful error, this check is the one that cannot be forgotten.
func (u *unit) move(e Entry) error {
	bonus := u.w.Bonus + e.Bonus
	purchased := u.w.Purchased + e.Purchased
	if bonus < 0 || purchased < 0 {
		return &InsufficientError{Required: -(e.Bonus + e.Purchased), Available: u.w.Total()}
	}
	if bonus > MaxAmount || purchased > MaxAmount {
		return invalid("a bucket would exceed MaxAmount")
	}
	u.w.Bonus, u.w.Purchased = bonus, purchased
	u.w.Seq++
	e.Account = u.w.Account
	e.Seq = u.w.Seq
	e.BonusAfter = bonus
	e.PurchasedAfter = purchased
	e.At = u.now
	u.entries = append(u.entries, e)
	u.dirty = true
	return nil
}

// replay looks for an entry with the key. It reports done when one exists
// and matches the request, and ErrKeyReused when one exists and does not.
func (u *unit) replay(key string, same func(Entry) bool) (bool, error) {
	e, ok, err := u.v.Entry(u.ctx, key)
	if err != nil || !ok {
		return false, err
	}
	if !same(e) {
		return true, fmt.Errorf("%w: %q", ErrKeyReused, key)
	}
	return true, nil
}

// expire writes off the bonus bucket if its expiry has passed.
func (u *unit) expire() error {
	exp := u.w.BonusExpiresAt
	if exp.IsZero() || exp.After(u.now) {
		return nil
	}
	if u.w.Bonus > 0 {
		err := u.move(Entry{
			Kind:  KindExpire,
			Key:   u.ownKey("expire"),
			Bonus: -u.w.Bonus,
			Ref:   exp.UTC().Format(time.RFC3339),
			Note:  "bonus expired",
		})
		if err != nil {
			return err
		}
	}
	u.w.BonusExpiresAt = time.Time{}
	u.dirty = true
	return nil
}

// extend moves the bonus expiry to at least until.
func (u *unit) extend(until time.Time) {
	if u.w.BonusExpiresAt.Before(until) {
		u.w.BonusExpiresAt = until
	}
}

// ownKey makes a key for an entry the ledger writes on its own. The sequence
// number the entry is about to take makes it unique within the account.
func (u *unit) ownKey(what string) string {
	return fmt.Sprintf("%s%s:%d", reservedPrefix, what, u.w.Seq+1)
}

func checkKey(key string) error {
	switch {
	case key == "":
		return invalid("empty key")
	case len(key) > MaxKeyLength:
		return invalid("key longer than %d bytes", MaxKeyLength)
	case strings.HasPrefix(key, reservedPrefix):
		return invalid("key prefix %q is reserved", reservedPrefix)
	}
	return nil
}

func checkCharge(c Charge) error {
	if err := checkKey(c.Key); err != nil {
		return err
	}
	return checkAmount("charge", c.Amount)
}

func checkAmount(what string, amount int64) error {
	switch {
	case amount <= 0:
		return invalid("%s amount must be positive, got %d", what, amount)
	case amount > MaxAmount:
		return invalid("%s amount %d is beyond MaxAmount", what, amount)
	}
	return nil
}
