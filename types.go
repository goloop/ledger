package ledger

import "time"

// Kind says what a ledger entry records.
//
// Kinds are strings rather than integers so that a row read straight out of
// the database is legible without this package, which is what an operator
// reconciling a wallet by hand at night actually has.
type Kind string

// The movements a wallet can make.
const (
	// KindGrant adds bonus credits with an expiry: a welcome grant, a
	// monthly allowance, a promotion.
	KindGrant Kind = "grant"

	// KindDeposit adds purchased credits, which never expire.
	KindDeposit Kind = "deposit"

	// KindHold takes the price of an action out of the buckets while the
	// action runs.
	KindHold Kind = "hold"

	// KindCapture settles a hold as spent. It moves nothing: the credits
	// already left the buckets with the hold.
	KindCapture Kind = "capture"

	// KindRelease returns a hold to the buckets it came from.
	KindRelease Kind = "release"

	// KindSpend charges an action at once, a hold and a capture in one row.
	KindSpend Kind = "spend"

	// KindReverse takes purchased credits back: a refund, a chargeback.
	KindReverse Kind = "reverse"

	// KindExpire writes off bonus credits whose period ended.
	KindExpire Kind = "expire"

	// KindAdjust is a manual correction by an operator.
	KindAdjust Kind = "adjust"
)

// Origin says why bonus credits were granted.
//
// The two are spent the same way and expire the same way. They differ in
// what the business owes: a promo grant is a gift and can be withheld next
// month, while a plan allowance is part of what a subscriber paid for and
// cannot be taken back at our discretion. Recording the origin on the grant
// keeps that distinction answerable after the fact.
type Origin string

// The origins of bonus credits.
const (
	// OriginPromo is a discretionary grant: welcome credits, a level bonus,
	// a goodwill gesture.
	OriginPromo Origin = "promo"

	// OriginPlan is the allowance a paid plan promises for its period.
	OriginPlan Origin = "plan"
)

// HoldStatus is where a hold is in its life.
type HoldStatus string

// The states of a hold. A hold is created held and ends exactly once, as
// captured or released.
const (
	HoldHeld     HoldStatus = "held"
	HoldCaptured HoldStatus = "captured"
	HoldReleased HoldStatus = "released"
)

// Wallet is the current state of one account: two buckets and the sequence
// number of the last entry that changed them.
//
// Amounts are integers in whatever unit the application chooses. Storing
// hundredths of a credit rather than whole credits is recommended, so that a
// percentage discount on a small price never rounds to zero.
type Wallet struct {
	// Account identifies the wallet's owner: a user, a team, a tenant.
	Account string

	// Bonus is the balance of credits that expire, and Purchased the balance
	// of credits that were paid for and never expire.
	Bonus     int64
	Purchased int64

	// BonusExpiresAt is when the whole bonus bucket expires. Whenever Bonus
	// is positive it is set; it is cleared when the bucket expires, and it
	// survives the bucket being spent down to zero, so bonus credits a
	// released hold brings back keep the expiry they left with.
	//
	// One expiry covers the whole bucket, so a new grant extends it rather
	// than starting a second clock. Grant.Replace is how a new period's
	// allowance replaces the previous one instead of adding to it.
	BonusExpiresAt time.Time

	// Seq is the sequence number of the last entry. Entries of one account
	// are numbered 1, 2, 3, ... without gaps, which is what lets Audit tell
	// a missing row from a merely old one.
	Seq int64

	// UpdatedAt is when the wallet last changed.
	UpdatedAt time.Time
}

// Total is what the wallet can spend now: both buckets together.
func (w Wallet) Total() int64 { return w.Bonus + w.Purchased }

// Entry is one row of the ledger: a movement and the balances after it.
//
// The ledger is append-only. An entry is never updated or deleted; a mistake
// is corrected by another entry. The balances after every movement are
// recorded next to it, so any row can be checked against the one before it
// without replaying the account from the start.
type Entry struct {
	// Account is the wallet the entry belongs to, and Seq its position in
	// that wallet's history (1 for the first entry).
	Account string
	Seq     int64

	// Kind is what the entry records.
	Kind Kind

	// Key makes the movement idempotent: an account never has two entries
	// with the same key, so repeating a request with the same key finds the
	// first entry instead of making a second one.
	Key string

	// Bonus and Purchased are the changes to the two buckets, signed.
	Bonus     int64
	Purchased int64

	// BonusAfter and PurchasedAfter are the balances after the movement.
	BonusAfter     int64
	PurchasedAfter int64

	// Hold is the id of the hold the entry belongs to, for hold, capture
	// and release entries.
	Hold string

	// Origin is set on grants.
	Origin Origin

	// Ref is the caller's reference: a feature, a transaction id, an order.
	// Note is a human explanation. Neither is interpreted.
	Ref  string
	Note string

	// At is when the entry was made.
	At time.Time
}

// Hold is an amount set aside for an action that has not finished.
//
// It remembers which buckets it drew from, so a release puts bonus credits
// back into the bonus bucket and purchased ones into the purchased bucket. A
// failed action therefore costs nothing, not even the conversion of bonus
// credits into purchased ones or back.
type Hold struct {
	// ID identifies the hold; Account is the wallet it drew from.
	ID      string
	Account string

	// Key is the key of the request that created the hold.
	Key string

	// Amount is the whole price; FromBonus and FromPurchased are its parts.
	Amount        int64
	FromBonus     int64
	FromPurchased int64

	// Ref is the caller's reference, copied from the charge.
	Ref string

	// Status is where the hold is; SettledAt is when it was captured or
	// released, zero while it is held.
	Status    HoldStatus
	CreatedAt time.Time
	SettledAt time.Time
}
