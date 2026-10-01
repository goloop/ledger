// Package ledger keeps prepaid credit wallets: two buckets per account, an
// append-only history of every movement, holds for actions in flight, and a
// key on every movement so that nothing is applied twice.
//
// It is the part of a billing system that has to be right before anything
// else is: the balance a customer paid for. What the credits buy, how they
// are sold and what a plan grants stay in the application; the ledger only
// moves credits and remembers why.
//
//	l := ledger.New(ledger.NewMemoryStore())
//
//	// The webhook of a paid transaction, however many times it arrives.
//	err := l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 1000_00})
//
//	// An action that costs 25 credits.
//	h, err := l.Hold(ctx, "user-42", ledger.Charge{Key: "job-7", Amount: 25_00, Ref: "pdf-to-word"})
//	if err != nil {
//	    return err // ErrHoldExists: this job already has a hold, do not run it twice
//	}
//	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
//	defer cancel()
//	if err := run(job); err != nil {
//	    _ = l.Release(settle, "user-42", h.ID) // a failure costs nothing
//	    return err
//	}
//	err = l.Capture(settle, "user-42", h.ID) // on an error, retry; never release
//
// # Two buckets
//
// Bonus credits expire: welcome grants, a monthly allowance, a plan's
// allowance for its period. Purchased credits were paid for and never
// expire. Spending takes bonus credits first, so what would be lost is used
// before what would not. A refund reverses purchased credits only, because
// bonus credits were never paid for.
//
// The bonus bucket has one expiry. A grant extends it and never shortens it;
// Grant.Replace writes off what is left and starts the new period instead,
// which is how a plan's allowance for a new period replaces the old one.
// Expiry is applied lazily: Balance shows an expired bucket as empty without
// writing, and the next movement records the write-off as an entry.
//
// # Keys
//
// Every movement carries a key, unique within the account. A repeated call
// with the same key changes nothing and reports what the first call did; a
// call that reuses a key for a different movement fails with ErrKeyReused.
// Keys come from the thing being paid for: the provider's transaction id for
// a deposit, its refund id for a reversal, the job id for a charge, the
// period for a monthly grant ("allowance:2026-09").
//
// # Holds
//
// Hold takes the price of an action out of the wallet while it runs; Capture
// makes it final and Release puts it back into the buckets it came from.
// Only the call that makes a hold gets no error; a repeat with the same key
// gets the hold back with ErrHoldExists, so the action runs once. Release
// belongs on the path where the action failed, never in a defer: a deferred
// Release after a Capture that failed gives the action away. A hold
// that is never settled - the process died - is released by ReleaseStale,
// run from a periodic worker. Spend is a hold and a capture in one step, for
// an action that cannot fail once it is paid for.
//
// # Reversals
//
// Reverse takes purchased credits back after a refund or a chargeback: what
// is left in the bucket, then the purchased share of holds still in flight,
// so a failed action cannot bring refunded credits back. What was already
// spent stays spent, and the entry records the shortfall. Reversal.Exact
// takes all or nothing from the free purchased credits. One refund is one
// reversal under one key, whichever path - a withdrawal or a webhook -
// applies it.
//
// # Amounts
//
// Amounts are int64 in a unit the application chooses, at most MaxAmount.
// Hundredths of a credit are recommended: a percentage discount on a
// one-credit price then still has somewhere to go. The ledger never converts money; the exchange
// rate between a currency and credits belongs to the application, and
// changing it after credits were sold devalues them.
//
// # Pricing models
//
// The ledger carries the models that spend a balance, and leaves prices,
// packs and plans to the application:
//
//   - credits, pay per action: Deposit a pack, Hold and Capture per action;
//   - priced items ("a gallery for 15 days: €1.50"): Spend from the wallet;
//   - a plan with an allowance: Grant with Replace and OriginPlan each month,
//     whatever the term the customer pays for (month, year, two years);
//   - a plan with overage: the allowance is spent first, purchased credits
//     after;
//   - free tiers and promotions: Grant with OriginPromo;
//   - refunds, withdrawals and chargebacks: Reverse.
//
// Unlimited plans, capacity rental ("50 GB for a year") and one-time
// licences are rights, not balances: they belong in the application's
// entitlements, and the ledger has nothing to count for them. DOC.md shows
// every model in code.
//
// # Storage
//
// A Store keeps wallets, entries and holds, and makes each update of an
// account atomic and serial. MemoryStore is the reference implementation;
// a database store is application code, and ledgertest.Store checks it
// against the same contract, concurrency included.
//
// The package depends only on the standard library.
package ledger
