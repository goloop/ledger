package ledger

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid reports a request the ledger refuses before looking at the
	// wallet: a missing key, a non-positive amount, an expiry in the past.
	// The returned error wraps it with the reason.
	ErrInvalid = errors.New("ledger: invalid request")

	// ErrInsufficient reports that the wallet cannot cover an amount. The
	// error returned is an *InsufficientError that matches it.
	ErrInsufficient = errors.New("ledger: insufficient credits")

	// ErrNotFound reports a hold that does not exist in the account.
	ErrNotFound = errors.New("ledger: hold not found")

	// ErrReleased is returned by Capture for a hold that was already
	// released - by the caller, or by ReleaseStale because the action took
	// longer than the stale limit. The credits went back to the wallet, so
	// the action was not paid for; the caller decides whether to charge it
	// again or let it go.
	ErrReleased = errors.New("ledger: hold already released")

	// ErrKeyReused reports a key that the account already used for a
	// different movement: another kind, another amount, another hold. A
	// retry must repeat the request exactly; a new request needs a new key.
	ErrKeyReused = errors.New("ledger: key already used for a different movement")

	// ErrDuplicateKey is returned by a Store when a change carries an entry
	// whose key the account already has. See Store.Update.
	ErrDuplicateKey = errors.New("ledger: duplicate entry key")

	// ErrCorrupt is returned by Audit when the stored history does not add
	// up. The returned error wraps it with the first discrepancy found.
	ErrCorrupt = errors.New("ledger: history does not add up")
)

// InsufficientError says how much was needed and how much there was.
type InsufficientError struct {
	Required  int64
	Available int64
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("ledger: insufficient credits: need %d, have %d", e.Required, e.Available)
}

// Is makes errors.Is(err, ErrInsufficient) true.
func (e *InsufficientError) Is(target error) bool { return target == ErrInsufficient }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}
