[![deps.dev](https://img.shields.io/badge/deps.dev-insights-4c8dbc)](https://deps.dev/go/github.com%2Fgoloop%2Fledger) [![Go Reference](https://img.shields.io/badge/godoc-reference-blue)](https://pkg.go.dev/github.com/goloop/ledger) [![License](https://img.shields.io/badge/license-MIT-brightgreen)](https://github.com/goloop/ledger/blob/master/LICENSE) [![Stay with Ukraine](https://img.shields.io/static/v1?label=Stay%20with&message=Ukraine%20♥&color=ffD700&labelColor=0057B8&style=flat)](https://u24.gov.ua/)

# ledger

`ledger` keeps prepaid credit wallets for a Go service: two buckets per
account (bonus credits that expire, purchased credits that never do), an
append-only history of every movement, holds for actions in flight, and a
key on every movement so that nothing is ever applied twice.

It is the engine under credits, priced items and plan allowances - the part
of billing that has to be right before anything else: the balance a
customer paid for. Standard library only.

Ukrainian version: **[README.UK.md](README.UK.md)**.

## The problem it solves

Every product that sells usage ends up writing the same wallet, and gets the
same things wrong:

- **Double credits.** The payment provider delivers a webhook twice, or a
  worker retries after a timeout, and the customer is credited twice.
- **Paid failures.** An action is charged, then fails, and the credits are
  gone; or they come back into the wrong bucket, turning a free bonus into
  paid credits.
- **Races.** Two requests of one user spend the same balance at once and the
  wallet goes negative.
- **Expiry that is not.** Bonus credits and plan allowances are supposed to
  expire, the monthly job misses a run, and they pile up.
- **Refunds after spending.** A chargeback arrives for credits that were
  already used, and the code either crashes or leaves a negative balance. Or
  it arrives while an action holds them, the action fails, and the refunded
  credits come back to the wallet.
- **No trail.** A support ticket asks "where did my 300 credits go" and the
  only answer is a number in a column.

`ledger` solves these once: every movement is keyed and idempotent, holds
settle exactly once, updates of one account are serialized by the store, the
bonus bucket expires by itself, reversals take what is left - holds in
flight included - and report the shortfall, and every movement is an entry with the balances after it - which
`Audit` can prove add up.

## Features

- **Two buckets.** Bonus credits (grants, plan allowances) expire and are
  spent first; purchased credits never expire and are the only ones a refund
  reverses.
- **Idempotent by key.** A repeated call changes nothing and reports what the
  first did; a key reused for a different movement is an error.
- **Holds.** `Hold`, then `Capture` or `Release`; a release restores the
  exact buckets. `ReleaseStale` cleans up after crashed workers.
- **Plan allowances.** `Grant.Replace` starts a new period instead of piling
  on the old one; `Origin` records whether bonus credits were a gift or part
  of a paid plan.
- **Reversals** for refunds and chargebacks: they reach holds in flight,
  report the shortfall, and take all or nothing for a withdrawal (`Exact`).
- **History** that is append-only, gapless and self-checking (`Audit`).
- **Pluggable storage.** A small `Store` interface; `MemoryStore` as the
  reference; `ledgertest.Store` checks a database store against the same
  contract, concurrency included.
- **Zero dependencies.**

## Installation

```bash
go get github.com/goloop/ledger
```

## Quick start

```go
l := ledger.New(ledger.NewMemoryStore()) // a database store in production

// A paid pack. The webhook may arrive three times; the key makes it once.
err := l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 1000_00})

// Welcome credits that expire in a month.
err = l.Grant(ctx, "user-42", ledger.Grant{
	Key: "welcome", Amount: 300_00, Until: time.Now().AddDate(0, 1, 0),
})

// An action that costs 25 credits: bonus first, then purchased.
h, err := l.Hold(ctx, "user-42", ledger.Charge{Key: "job-7", Amount: 25_00, Ref: "pdf-to-word"})
if errors.Is(err, ledger.ErrInsufficient) {
	return errPaymentRequired // 402
}
if err != nil {
	return err
}
settle := context.WithoutCancel(ctx) // a dropped client must not undo a settlement
if err := run(job); err != nil {
	_ = l.Release(settle, "user-42", h.ID) // a failure costs nothing
	return err
}
// The action ran: on an error, retry the Capture. Never release here, and
// never defer the Release - that would give the action away.
err = l.Capture(settle, "user-42", h.ID)
```

Amounts are in a unit you choose; hundredths of a credit are recommended, so
`1000_00` is 1000 credits.

## Pricing models

The ledger does not know prices or plans; it moves credits. These are the
models it carries, and what stays in your application:

| Model | Example | The ledger does | Your application does |
|---|---|---|---|
| **Credits, pay per action** | API calls, conversions, AI requests | `Deposit` a pack, `Hold`/`Capture` per action | price book, packs, exchange rate |
| **Priced items** | "Gallery 25 GB x 15 days: €1.50" | `Spend` the item's price from the wallet | the item and its lifetime |
| **Plan with an allowance** | "Pro: 1000 credits a month" | `Grant{Replace, OriginPlan}` each period | the subscription and its rights |
| **Plan + overage** | allowance first, then paid credits | bonus is spent first, purchased after | nothing more |
| **Monthly / yearly / two-year terms** | $15 / $60 / $100 | the allowance per month, whatever the term | the term, via the payment provider |
| **Free tier, promotions** | welcome credits, monthly bonus | `Grant{OriginPromo}` with an expiry | who earns it |
| **Team wallet** | one balance for a team | the account is the team id | who may spend |
| **Refund, withdrawal, chargeback** | 14-day right of withdrawal | `Reverse`, with the shortfall | the rules of eligibility |

What the ledger is **not** for: an **unlimited** plan (that is an entitlement
plus a rate limit, nothing to count), **capacity rental** such as "50 GB for
a year" (a limit on what exists, not a balance that is spent), and a
**one-time licence** (a right, not credits). Those are rows in your
entitlements table; the payment provider sells them and the ledger stays out
of the way. The reference shows each model in code:
[Pricing models](DOC.md#pricing-models).

## A real-world example

Paddle sells a credit pack; the webhook credits it, and takes it back when
the money goes back.

```go
wh := paddle.NewWebhook(os.Getenv("PADDLE_WEBHOOK_SECRET"))

http.HandleFunc("POST /webhooks/paddle", func(w http.ResponseWriter, r *http.Request) {
	ev, err := wh.ParseRequest(r)
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	if err := handle(r.Context(), ev); err != nil {
		http.Error(w, "retry", http.StatusInternalServerError) // Paddle sends it again
		return
	}
	w.WriteHeader(http.StatusOK)
})

// handle is safe to run twice for one event: every ledger call is keyed.
func handle(ctx context.Context, ev *paddle.Event) error {
	switch ev.Type {
	case paddle.EventTransactionCompleted:
		tx, err := ev.Transaction()
		if err != nil {
			return err
		}
		p, err := purchases.ByTransaction(ctx, tx.ID) // your table
		if err != nil {
			return err
		}
		// The transaction id is the key: a repeated delivery is a no-op.
		return l.Deposit(ctx, p.Account, ledger.Deposit{Key: tx.ID, Amount: p.Credits, Ref: tx.ID})

	case paddle.EventAdjustmentCreated, paddle.EventAdjustmentUpdated:
		adj, err := ev.Adjustment()
		if err != nil {
			return err
		}
		if adj.Action != paddle.ActionRefund && adj.Action != paddle.ActionChargeback ||
			adj.Status != paddle.AdjustmentApproved {
			return nil
		}
		p, err := purchases.ByTransaction(ctx, adj.TransactionID)
		if err != nil {
			return err
		}
		// The adjustment id is the key: created and updated may both arrive.
		_, err = l.Reverse(ctx, p.Account, ledger.Reversal{
			Key: adj.ID, Amount: p.Credits, Ref: adj.TransactionID,
		})
		return err
	}
	return nil
}
```

## The API at a glance

| Call | What it does |
|---|---|
| `New(store, opts...)` | a ledger over a store |
| `Grant` | add bonus credits with an expiry (`Replace` for a new period) |
| `Deposit` | add purchased credits |
| `Hold` / `Capture` / `Release` | set a price aside, then settle it |
| `Spend` | charge at once |
| `Reverse` | take purchased credits back after a refund, holds in flight included |
| `Adjust` | a manual correction |
| `Balance` | the wallet now, expiry applied |
| `Entries` | the history, newest first, paged |
| `Audit` | prove the history adds up |
| `ReleaseStale` | release holds left by crashed workers |
| `NewMemoryStore` | the reference store |
| `ledgertest.Store` | the conformance suite for your store |

## Documentation

- Full reference, pricing models and the store contract:
  [DOC.md](DOC.md) · [DOC.UK.md](DOC.UK.md)
- Package API: [pkg.go.dev/github.com/goloop/ledger](https://pkg.go.dev/github.com/goloop/ledger)
- Changes between versions: [CHANGELOG.md](CHANGELOG.md)
- Payment providers: [goloop/paddle](https://github.com/goloop/paddle),
  [goloop/creem](https://github.com/goloop/creem)

## Contributing

Contributions are welcome. Please run `go test -race ./...`, `go vet ./...`
and `gofmt -l .` before submitting a pull request.

## License

`ledger` is released under the MIT License. See [LICENSE](LICENSE).
