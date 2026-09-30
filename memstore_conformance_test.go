package ledger_test

import (
	"testing"

	"github.com/goloop/ledger"
	"github.com/goloop/ledger/ledgertest"
)

func TestMemoryStoreConformance(t *testing.T) {
	ledgertest.Store(t, func(*testing.T) ledger.Store { return ledger.NewMemoryStore() })
}
