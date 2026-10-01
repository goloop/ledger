# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - 2026-10-01

### Changed
- `Hold` returns no error only to the call that makes the hold. A repeat
  with the same key gets the hold back with `ErrHoldExists` (and
  `ErrCaptured` or `ErrReleased` once it is settled), so a retried request
  or a second worker on the same job never runs the action twice.
- `Store.StaleHolds` takes a `StaleQuery` and continues after a
  `(created_at, id)` pair; `ReleaseStale` walks past holds that keep
  failing instead of reading the same first page again.
- Bonus credits a hold took from a period that `Grant.Replace` ended are
  written off when the hold is released, instead of joining the new period.
  `Wallet.BonusPeriod` and `Hold.BonusPeriod` record the period.

### Fixed
- A repeated `Reverse` compares `Exact` too (`Entry.Exact`): the same key
  with another mode is `ErrKeyReused`.
- The documentation keys every refund by one record of the application, so
  a withdrawal and the provider's webhook for it reverse once, not twice.
- The documentation no longer presents `Exact` as proof that a pack is
  unused: it checks the free purchased credits.
- The hold pattern settles with its own timeout, keeps failed captures for a
  settlement worker, and answers a repeated job from the job's record.

### Added
- `ErrHoldExists`, `ErrCaptured`, `StaleQuery`.
- The documented schema gains `bonus_period` on wallets and holds and
  `exact` on entries; the stale-holds index covers `(created_at, id)`.

## [0.2.0] - 2026-10-01

### Fixed
- `Reverse` reaches the purchased share of holds in flight. Before, a refund
  that arrived while an action held the credits took nothing from the hold,
  and a later `Release` gave the refunded credits back. Each hold records
  the part taken in `Hold.Reversed`, and a release returns only the rest.
- `Hold` repeated with the key of a released hold returns it with
  `ErrReleased` instead of no error, so the action is not run on a hold
  that no longer pays for it.
- `Audit` checks the entries up to the wallet's `Seq`, so a movement made
  during the audit no longer looks like corruption. It walks the history a
  page at a time instead of holding it in memory, and also catches a wallet
  behind its history and a history that does not start from zero.
- `ReleaseStale` goes past a hold that fails to release and returns the
  failures joined, instead of stopping at the first one; its limit is
  capped at 1000.
- Amounts are bounded by `MaxAmount` (2^53 - 1): a movement that would take
  a bucket beyond it is `ErrInvalid` instead of an overflow reported as
  `ErrInsufficient`.
- A repeated `Grant` compares the origin, and a repeated `Reverse` the
  amount, so a key reused for a different movement is `ErrKeyReused`.
- `MemoryStore.Entries` with a negative limit returns nothing instead of
  panicking.
- The documented hold pattern settles on `context.WithoutCancel` and
  releases on the failure path only: a deferred `Release` after a failed
  `Capture` gave the action away.

### Added
- `Reversal.Exact`: take the whole amount from the purchased bucket or
  nothing, for the withdrawal of an unused pack.
- `Entry.Requested`, `Entry.FromHolds`, `Entry.Taken` and `Entry.Short`: a
  reversal records what it asked for and what it took as numbers, not in
  the note.
- `View.Held`, the account's open holds, in the `Store` contract and in
  `ledgertest.Store`. The documented schema gains `requested`, `from_holds`
  and `reversed`, `CHECK` constraints on holds and a trigger that keeps the
  history append-only.

## [0.1.0] - 2026-09-30

First release.

### Added
- First version: two-bucket wallets (bonus with one expiry, purchased
  without), `Grant` with `Replace` and `Origin`, `Deposit`, `Hold` /
  `Capture` / `Release`, `Spend`, `Reverse` with shortfall, `Adjust`,
  `Balance`, `Entries`, `Audit`, `ReleaseStale`.
- An idempotency key on every movement, with `ErrKeyReused` for a key used
  for a different movement.
- The `Store` contract, `MemoryStore` as its reference, and
  `ledgertest.Store` to check a database store against it.
- README and reference in English and Ukrainian: the problem the package
  solves, the pricing and selling models it supports, a real-world example.
