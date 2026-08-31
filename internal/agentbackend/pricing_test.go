package agentbackend

import "testing"

func TestPricingTableCostPreservesExactOverlappingModel(t *testing.T) {
	table := PricingTable{
		Models: map[string]Price{
			"gpt-4":  {InputPerMTok: 1},
			"gpt-4o": {InputPerMTok: 2},
		},
		Default: Price{InputPerMTok: 3},
	}

	if got := table.Cost("gpt-4o", 1_000_000, 0); got != 2 {
		t.Fatalf("Cost(gpt-4o) = %v, want exact model cost 2", got)
	}
}
