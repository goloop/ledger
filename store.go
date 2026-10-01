package ledger

import (
	"context"
	"time"
)

// Store keeps wallets, entries and holds.
//
// The ledger decides what a movement does; the store only has to keep what it
// is told, atomically, one account at a time. That split is what makes the
// rules testable once, here, and the storage a small amount of application
// code: a store never computes a balance, never decides whether an amount is
// affordable and never interprets a key.
//
// MemoryStore is the reference implementation. A store backed by a database
// is checked against the same contract by ledgertest.Store.
type Store interface {
	// Wallet returns the account's wallet as stored, without locking it. An
	// account that has never had an entry is a zero Wallet carrying the
	// account id, not an error.
	Wallet(ctx context.Context, account string) (Wallet, error)

	// Update is the only way anything is written.
	//
	// It locks the account's wallet (creating it when the account is new),
	// calls fn with a View of the locked state, and if fn returns a non-nil
	// Change, writes all of it atomically before releasing the lock: the
	// wallet replaced, the entries appended, the holds inserted or replaced
	// by id. If fn returns an error or a nil Change, nothing is written and
	// the error, if any, is returned as it is.
	//
	// Two updates of one account never overlap. Updates of different
	// accounts may run in parallel.
	//
	// fn may be called more than once, when a store retries a transaction
	// its database aborted; it has no side effects of its own.
	//
	// An entry whose key the account already has must fail the whole
	// change with ErrDuplicateKey. The ledger checks keys before writing,
	// so this only fires when the check and the write were not in one
	// transaction - which is a bug in the store, and the reason the rule
	// is there.
	Update(ctx context.Context, account string, fn func(View) (*Change, error)) error

	// Entries returns up to limit entries of the account, newest first,
	// whose Seq is below before. before <= 0 means from the newest.
	Entries(ctx context.Context, account string, before int64, limit int) ([]Entry, error)

	// StaleHolds returns up to q.Limit holds, of any account, that are still
	// held and were created before q.CreatedBefore, ordered by CreatedAt
	// and then ID. With q.AfterID set it returns only the holds after
	// (q.AfterCreated, q.AfterID) in that order, so a walk can go past holds
	// that keep failing instead of reading them again.
	StaleHolds(ctx context.Context, q StaleQuery) ([]Hold, error)
}

// StaleQuery selects the holds StaleHolds returns.
type StaleQuery struct {
	// CreatedBefore keeps the holds created before it.
	CreatedBefore time.Time

	// AfterCreated and AfterID continue a walk from the last hold of the
	// previous page; an empty AfterID starts from the oldest.
	AfterCreated time.Time
	AfterID      string

	// Limit is the most holds to return.
	Limit int
}

// View is the locked state of one account inside Store.Update.
type View interface {
	// Wallet is the wallet as it was when the lock was taken.
	Wallet() Wallet

	// Entry finds the account's entry with the given key.
	Entry(ctx context.Context, key string) (Entry, bool, error)

	// Hold finds the account's hold with the given id.
	Hold(ctx context.Context, id string) (Hold, bool, error)

	// Held returns the account's holds that are still held, oldest first.
	// A reversal reads them to take back purchased credits that are in
	// flight; an account rarely has more than a few.
	Held(ctx context.Context) ([]Hold, error)
}

// Change is what one Update writes.
type Change struct {
	// Wallet replaces the stored wallet.
	Wallet Wallet

	// Entries are appended, in order. Their Seq values continue the
	// wallet's history and the last one equals Wallet.Seq.
	Entries []Entry

	// Holds are inserted, or replaced when a hold with the same id exists.
	Holds []Hold
}
