# ledger - reference

The full reference for the `ledger` package: wallets, keys, grants, deposits,
holds, reversals, adjustments, the history and its audit, and the storage
contract.

Ukrainian version: **[DOC.UK.md](DOC.UK.md)**.

## Contents

- [Mental model](#mental-model)
- [Creating a ledger](#creating-a-ledger)
- [Amounts](#amounts)
- [Keys](#keys)
- [Grants and expiry](#grants-and-expiry)
- [Deposits](#deposits)
- [Holds](#holds)
- [Spend](#spend)
- [Reversals](#reversals)
- [Adjustments](#adjustments)
- [Balance and history](#balance-and-history)
- [Audit](#audit)
- [Stale holds](#stale-holds)
- [Pricing models](#pricing-models)
- [Writing a store](#writing-a-store)
- [Errors](#errors)
- [Scope](#scope)
- [Dependencies](#dependencies)

## Mental model

An account has a **wallet** with two buckets:

| Bucket | Filled by | Expires | Spent | Reversed by a refund |
|---|---|---|---|---|
| `Bonus` | `Grant` | yes, one expiry for the bucket | first | never |
| `Purchased` | `Deposit` | never | second | yes |

Every change is an **entry** in an append-only history: what moved, the
balances after it, the key that made it idempotent, and a reference. Entries
of one account are numbered 1, 2, 3, ... without gaps. The wallet is the sum
of its history, and `Audit` checks that it is.

A **hold** is an amount set aside for an action that has not finished. It
remembers which buckets it drew from, so a release restores exactly what was
taken.

The ledger never computes a price, never converts money and never talks to a
payment provider. It is the part of billing that must be right before
anything else is: the balance a customer paid for.

## Creating a ledger

```go
l := ledger.New(store)

l = ledger.New(store,
	ledger.WithClock(clock.Now),            // tests
	ledger.WithReleaseGrace(24*time.Hour),  // see Holds
)
```

A `Ledger` is safe for concurrent use. The store serializes updates of one
account; updates of different accounts run in parallel.

## Amounts

Amounts are `int64` in a unit the application chooses. Store hundredths of a
credit rather than whole credits: a 15% discount on a one-credit price then
still has somewhere to go instead of rounding to zero.

The exchange rate between money and credits is the application's, and it
should be fixed once credits are sold: changing it devalues what customers
already hold.

## Keys

Every method that changes a wallet takes a key, unique within the account.

- A repeated call with the same key changes nothing and reports what the
  first call did: `Hold` returns the same hold, `Reverse` the same amount.
- A call that reuses a key for a different movement - another kind, another
  amount, another reference - fails with `ErrKeyReused`. A retry must repeat
  the request exactly; a new request needs a new key.
- Keys are at most `MaxKeyLength` (255) bytes. The prefix `ledger:` is
  reserved for the entries the ledger writes itself (captures, releases,
  expiries).

Take keys from the thing being paid for:

| Movement | Key |
|---|---|
| Deposit for a payment | the provider's transaction id |
| Reversal for a refund | the provider's refund or adjustment id, not the transaction id (one transaction can be refunded in parts) |
| Charge for an action | the job, request or order id |
| Monthly allowance | the period: `allowance:2026-09` |
| Welcome grant | `welcome` |

## Grants and expiry

```go
err := l.Grant(ctx, account, ledger.Grant{
	Key:    "welcome",
	Amount: 300_00,
	Until:  time.Now().AddDate(0, 1, 0),
	Origin: ledger.OriginPromo, // the default
	Note:   "welcome credits",
})
```

The bonus bucket has **one expiry**. A grant moves it to `Until` if that is
later and never earlier, so a short promotion does not cut a longer grant
short.

A plan's allowance for a new period should replace the previous one, not add
to it. `Replace` writes off what is left of the bucket (an `expire` entry)
and sets the expiry to `Until`:

```go
err := l.Grant(ctx, team, ledger.Grant{
	Key:     "allowance:" + period.Format("2006-01"),
	Amount:  plan.MonthlyCredits,
	Until:   period.AddDate(0, 1, 0),
	Replace: true,
	Origin:  ledger.OriginPlan,
})
```

`Origin` separates a gift (`OriginPromo`: welcome, level bonus, goodwill)
from what a subscriber paid for (`OriginPlan`). Both expire and are spent the
same way; the difference is what the business owes, and the origin keeps it
answerable later.

Expiry is **lazy**. `Balance` shows a bucket past its expiry as empty without
writing anything; the next movement of the account writes the `expire` entry
first. A balance page read a thousand times a day takes no lock.

## Deposits

```go
err := l.Deposit(ctx, account, ledger.Deposit{
	Key:    tx.ID,
	Amount: pack.Credits,
	Ref:    tx.ID,
})
```

Purchased credits never expire. Call `Deposit` from the webhook that reports
the payment; a repeated delivery is a no-op.

## Holds

```go
h, err := l.Hold(ctx, account, ledger.Charge{Key: job.ID, Amount: price, Ref: "pdf-to-word"})
if err != nil {
	return err // *InsufficientError when the wallet cannot cover it
}
defer l.Release(ctx, account, h.ID)

if err := run(job); err != nil {
	return err
}
return l.Capture(ctx, account, h.ID)
```

- `Hold` takes the price out of the buckets, bonus first, and returns the
  hold with `FromBonus` and `FromPurchased`.
- `Capture` makes it final. Capturing twice is a no-op.
- `Release` puts it back into the buckets it came from. Releasing twice is a
  no-op, and releasing a captured hold is a no-op too, which makes the
  deferred release above correct without a condition.
- `Capture` of a released hold fails with `ErrReleased`: the credits are
  already back, and the action ran unpaid. The caller decides whether to
  charge again or let it go.

When bonus credits come back after the bucket expired while the action ran,
they get a grace period (`DefaultReleaseGrace`, 24 hours) instead of arriving
already expired. Bonus credits that come back before the expiry keep it.

## Spend

```go
err := l.Spend(ctx, account, ledger.Charge{Key: req.ID, Amount: price, Ref: "tts"})
```

A hold and a capture in one entry, for an action that cannot fail once it is
paid for.

## Reversals

```go
taken, err := l.Reverse(ctx, account, ledger.Reversal{
	Key:    adjustment.ID,
	Amount: purchase.Credits,
	Ref:    adjustment.TransactionID,
	Note:   "chargeback",
})
if taken < purchase.Credits {
	// the customer already spent part of it: record the loss
}
```

A reversal takes back purchased credits after the money went back. It takes
what is left, up to the amount; credits already spent stay spent, and the
shortfall is returned and noted on the entry. Bonus credits are never taken.

## Adjustments

```go
err := l.Adjust(ctx, account, ledger.Adjustment{
	Key:       "ticket-4711",
	Purchased: 50_00,
	Note:      "support: failed conversion charged twice",
})
```

A manual correction by an operator: positive amounts add, negative ones take
away, and neither bucket can go below zero. Adding bonus credits to a bucket
without an expiry needs `BonusUntil`.

## Balance and history

```go
w, err := l.Balance(ctx, account)         // Bonus, Purchased, BonusExpiresAt, Seq
page, err := l.Entries(ctx, account, 0, 50) // newest first
next, err := l.Entries(ctx, account, page[len(page)-1].Seq, 50)
```

An account that has never had an entry reads as an empty wallet, not an
error.

## Audit

```go
if err := l.Audit(ctx, account); errors.Is(err, ledger.ErrCorrupt) {
	// the history does not add up: alert, do not auto-fix
}
```

`Audit` replays the whole history: gapless numbering, every recorded balance
equal to the previous one plus the movement, no bucket below zero, and the
last balance equal to the wallet. It reads every entry of the account, so run
it from a reconciliation job, not from a request.

## Stale holds

```go
n, err := l.ReleaseStale(ctx, 30*time.Minute, 100)
```

Releases holds that have been held longer than the age - left behind by a
process that died between the hold and its settlement. Run it from a periodic
worker, with an age comfortably longer than the slowest action.

## Pricing models

The ledger moves credits; prices, packs, plans and their rights belong to the
application. This section shows how each common model sits on top of it, and
where the ledger is the wrong tool.

Two tables do the rest of the work in every model below:

- **purchases** - one row per checkout: account, what was bought, amount,
  credits, provider transaction id, status;
- **entitlements** - what an account may do now: plan, its limits, until
  when. The provider is the source of truth for money, this table for rights.

### Credits, pay per action

The customer buys a pack; every action has a price in credits, shown before
it runs. Heavy users pay more, light users pay little, a month without use
costs nothing.

```go
// Checkout: the amount comes from your pack table, never from the client.
// Webhook, transaction completed:
err := l.Deposit(ctx, p.Account, ledger.Deposit{Key: tx.ID, Amount: p.Credits, Ref: tx.ID})

// Each action:
price := priceBook[feature] // 0 means free: do not touch the ledger
h, err := l.Hold(ctx, account, ledger.Charge{Key: job.ID, Amount: price, Ref: feature})
// ... run, then Capture or Release
```

A volume discount is more credits for the money (`Credits` of a bigger pack),
not a cheaper credit: the value of one credit stays fixed, so a price in
credits always has one answer in money.

### Priced items

The customer sees a thing with a price - "Gallery, 25 GB for 15 days:
€1.50" - and pays it from the wallet. Credits stay in the background; the
wallet is how small prices avoid a card fee each.

```go
err := l.Spend(ctx, account, ledger.Charge{
	Key:    "gallery:" + g.ID + ":create",
	Amount: item.Price,
	Ref:    "gallery-25gb-15d",
})
if err == nil {
	err = galleries.Activate(ctx, g.ID, time.Now().AddDate(0, 0, 15))
}
```

An extension is another item with its own key (`gallery:<id>:extend:2`).

### Plan with an allowance

A subscription grants credits for each period; they expire with the period
and do not pile up. The subscription itself lives with the payment provider;
the ledger holds the allowance.

```go
func grantAllowance(ctx context.Context, sub Subscription, month time.Time) error {
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	return l.Grant(ctx, sub.Account, ledger.Grant{
		Key:     "allowance:" + sub.ID + ":" + start.Format("2006-01"),
		Amount:  sub.Plan.MonthlyCredits,
		Until:   start.AddDate(0, 1, 0),
		Replace: true,
		Origin:  ledger.OriginPlan,
	})
}
```

Call it when the subscription becomes active (the webhook) and from a daily
job for every subscription whose paid period covers today. The key makes the
daily job safe to run as often as you like; `Replace` writes off last month's
leftover; `OriginPlan` records that this was paid for, so it is never taken
back at your discretion.

### Plan + overage

Nothing extra: the allowance is bonus credits and is spent first, purchased
credits from packs pay for the rest. A subscriber who needs more buys a pack;
a customer without a plan uses packs only.

### Monthly, yearly and two-year terms

The term is how the customer pays - $15 a month, $60 a year, $100 for two
years - and it belongs to the provider's subscription (`billing_cycle` in
Paddle, the product's billing period in Creem). The allowance should still be
granted **monthly**, whatever the term: a yearly plan that granted twelve
months at once would let a customer spend a year's allowance in a week and
cancel. The recipe above already does this - it grants per month while the
paid period lasts.

Prepaid long terms are a liability: a two-year plan is service owed for two
years, and closing the product early means refunding the unused part.

### Free tier and promotions

```go
// Welcome credits after the email is confirmed.
_ = l.Grant(ctx, account, ledger.Grant{Key: "welcome", Amount: 300_00, Until: now.AddDate(0, 1, 0)})

// A monthly bonus for customers who have paid and kept a purchase.
_ = l.Grant(ctx, account, ledger.Grant{
	Key: "bonus:" + now.Format("2006-01"), Amount: 100_00, Until: nextMonth,
})
```

Who earns a bonus - a confirmed email, a purchase past its withdrawal period,
a spending level - is policy and stays in the application. A daily free
quota per tool ("3 conversions a day") is a rate limit, not credits.

### Team wallet

The account is whatever owns the balance: a user id, a team id, a tenant id.
Who may spend from it is the application's permission check before `Hold`.

### Withdrawal, refunds and chargebacks

```go
// The 14-day withdrawal for an unused pack: the credits must still be there.
w, _ := l.Balance(ctx, p.Account)
if withinWithdrawal(p) && w.Purchased >= p.Credits {
	_, err = l.Reverse(ctx, p.Account, ledger.Reversal{
		Key: "withdraw:" + p.ID, Amount: p.Credits, Ref: p.TransactionID,
	})
	// then ask the provider for the refund; retry it from a worker if it fails
}

// A refund or chargeback started at the provider: take back what is left.
taken, err := l.Reverse(ctx, p.Account, ledger.Reversal{
	Key: adjustment.ID, Amount: p.Credits, Ref: p.TransactionID, Note: "chargeback",
})
```

The reversal key is the provider's refund or adjustment id, so a repeated
webhook takes nothing more.

### Where the ledger is the wrong tool

- **Unlimited plans** ("$60 a year, unlimited conversions, at most N per
  hour"): there is nothing to count. Check the entitlement, apply a rate
  limit, and leave the ledger out.
- **Capacity rental** ("50 GB for a year, as many galleries as you like"):
  capacity is a limit on what exists, not a balance that is spent; freeing a
  file gives the space back. Keep the limit on the entitlement and compare
  it with what is stored.
- **One-time licences** ("Maker, $59, updates for 12 months"): a right with
  a date, sold by the provider, kept in the entitlements table.

The same product can combine them: a capacity plan with credits for paid
extras such as generated images, an unlimited plan with a paid add-on. The
ledger then carries only the part that is spent.

## Writing a store

`MemoryStore` is for tests and single-process tools. A service keeps the
ledger in its database, and that store is application code - the package
takes no dependency on a driver.

The contract is small:

- **`Update` is atomic and serial per account.** Lock the wallet, call `fn`,
  write its `Change` (wallet replaced, entries appended, holds upserted by
  id) in the same transaction. Two updates of one account must never
  interleave.
- **A new account is locked too.** Create the wallet row before locking it,
  or two first updates race past each other.
- **Keys are unique per account** and the database enforces it: a change with
  a key the account already has fails whole with `ErrDuplicateKey`.
- **`fn` may run more than once** if the database aborts and the store
  retries; it has no side effects.
- **An error from `fn`, or a nil `Change`, writes nothing.**

In PostgreSQL the lock is `SELECT ... FOR UPDATE` on the wallet row, after an
`INSERT ... ON CONFLICT DO NOTHING` that makes the row exist:

```sql
CREATE TABLE ledger_wallets (
    account          text PRIMARY KEY,
    bonus            bigint NOT NULL DEFAULT 0 CHECK (bonus >= 0),
    purchased        bigint NOT NULL DEFAULT 0 CHECK (purchased >= 0),
    bonus_expires_at timestamptz,
    seq              bigint NOT NULL DEFAULT 0,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    account         text   NOT NULL,
    seq             bigint NOT NULL,
    kind            text   NOT NULL,
    key             text   NOT NULL,
    bonus           bigint NOT NULL,
    purchased       bigint NOT NULL,
    bonus_after     bigint NOT NULL,
    purchased_after bigint NOT NULL,
    hold            text   NOT NULL DEFAULT '',
    origin          text   NOT NULL DEFAULT '',
    ref             text   NOT NULL DEFAULT '',
    note            text   NOT NULL DEFAULT '',
    at              timestamptz NOT NULL,
    PRIMARY KEY (account, seq),
    UNIQUE (account, key)
);

CREATE TABLE ledger_holds (
    id             text PRIMARY KEY,
    account        text   NOT NULL,
    key            text   NOT NULL,
    amount         bigint NOT NULL,
    from_bonus     bigint NOT NULL,
    from_purchased bigint NOT NULL,
    ref            text   NOT NULL DEFAULT '',
    status         text   NOT NULL,
    created_at     timestamptz NOT NULL,
    settled_at     timestamptz
);
CREATE INDEX ledger_holds_stale ON ledger_holds (created_at) WHERE status = 'held';
```

The primary key on `(account, seq)` is a second line of defence: a store
that forgot the lock fails loudly on it instead of losing a movement.

Then prove it:

```go
func TestPostgresStore(t *testing.T) {
	ledgertest.Store(t, func(t *testing.T) ledger.Store {
		return newPostgresStore(t) // empty, cleaned up through t
	})
}
```

`ledgertest.Store` checks every rule above, including concurrent updates of
one account; a store without the row lock fails it.

## Errors

| Error | Meaning |
|---|---|
| `ErrInvalid` | refused before looking at the wallet: empty or reserved key, non-positive amount, expiry in the past |
| `ErrInsufficient` / `*InsufficientError` | the wallet cannot cover the amount; `Required` and `Available` say by how much |
| `ErrNotFound` | the hold does not exist in the account |
| `ErrReleased` | `Capture` of a hold that was already released |
| `ErrKeyReused` | the key was used for a different movement |
| `ErrDuplicateKey` | returned by a `Store`; means the store checked and wrote in separate transactions |
| `ErrCorrupt` | `Audit` found a history that does not add up |

Errors from the store are returned as they are.

## Scope

Not in this package, on purpose: prices, exchange rates, credit packs, plans,
welcome and monthly bonus rules, the right of withdrawal, payment providers
and webhooks. Those differ per product and change with the business; the
ledger underneath them should not.

## Dependencies

The standard library only.
