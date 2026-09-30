# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
