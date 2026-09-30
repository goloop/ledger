package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goloop/ledger"
)

func Example() {
	ctx := context.Background()
	l := ledger.New(ledger.NewMemoryStore())

	// Welcome credits for a month, and a paid pack. Amounts are hundredths
	// of a credit.
	_ = l.Grant(ctx, "user-42", ledger.Grant{Key: "welcome", Amount: 300_00, Until: time.Now().AddDate(0, 1, 0)})
	_ = l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 1000_00, Ref: "txn_01"})

	// The same webhook delivered again changes nothing.
	_ = l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 1000_00, Ref: "txn_01"})

	// An action that costs 350 credits: bonus first, then purchased.
	h, _ := l.Hold(ctx, "user-42", ledger.Charge{Key: "job-7", Amount: 350_00, Ref: "pdf-to-word"})
	fmt.Println("held:", h.FromBonus/100, "bonus,", h.FromPurchased/100, "purchased")
	_ = l.Capture(ctx, "user-42", h.ID)

	w, _ := l.Balance(ctx, "user-42")
	fmt.Println("balance:", w.Bonus/100, "bonus,", w.Purchased/100, "purchased")
	// Output:
	// held: 300 bonus, 50 purchased
	// balance: 0 bonus, 950 purchased
}

func ExampleLedger_Hold() {
	ctx := context.Background()
	l := ledger.New(ledger.NewMemoryStore())
	_ = l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 100})

	h, err := l.Hold(ctx, "user-42", ledger.Charge{Key: "job-8", Amount: 40})
	if err != nil {
		return
	}
	// Release unless captured: a no-op after a successful Capture.
	defer l.Release(ctx, "user-42", h.ID)

	if err := convert(); err != nil {
		return // the deferred Release gives the 40 back
	}
	_ = l.Capture(ctx, "user-42", h.ID)

	w, _ := l.Balance(ctx, "user-42")
	fmt.Println(w.Purchased)
	// Output: 60
}

func convert() error { return nil }

func ExampleLedger_Grant_allowance() {
	ctx := context.Background()
	start := time.Date(2030, 9, 1, 0, 0, 0, 0, time.UTC)
	l := ledger.New(ledger.NewMemoryStore(), ledger.WithClock(func() time.Time { return start }))

	// A plan's allowance for September, then for October: the new period
	// replaces what is left of the old one instead of piling on top of it.
	for _, month := range []time.Time{start, start.AddDate(0, 1, 0)} {
		_ = l.Grant(ctx, "team-7", ledger.Grant{
			Key:     "allowance:" + month.Format("2006-01"),
			Amount:  1000,
			Until:   month.AddDate(0, 1, 0),
			Replace: true,
			Origin:  ledger.OriginPlan,
		})
	}
	w, _ := l.Balance(ctx, "team-7")
	fmt.Println(w.Bonus, w.BonusExpiresAt.Format("2006-01-02"))
	// Output: 1000 2030-11-01
}

func ExampleLedger_Reverse() {
	ctx := context.Background()
	l := ledger.New(ledger.NewMemoryStore())
	_ = l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 1000})
	_ = l.Spend(ctx, "user-42", ledger.Charge{Key: "job-9", Amount: 700})

	// A chargeback for the whole pack: what is left comes back, what was
	// spent stays spent, and the shortfall is reported.
	taken, _ := l.Reverse(ctx, "user-42", ledger.Reversal{Key: "adj_01", Amount: 1000, Ref: "txn_01"})
	fmt.Println("taken:", taken, "short:", 1000-taken)
	// Output: taken: 300 short: 700
}

func ExampleInsufficientError() {
	ctx := context.Background()
	l := ledger.New(ledger.NewMemoryStore())
	_ = l.Deposit(ctx, "user-42", ledger.Deposit{Key: "txn_01", Amount: 10})

	err := l.Spend(ctx, "user-42", ledger.Charge{Key: "job-1", Amount: 25})
	var ie *ledger.InsufficientError
	if errors.As(err, &ie) {
		fmt.Printf("need %d, have %d\n", ie.Required, ie.Available)
	}
	fmt.Println(errors.Is(err, ledger.ErrInsufficient))
	// Output:
	// need 25, have 10
	// true
}
