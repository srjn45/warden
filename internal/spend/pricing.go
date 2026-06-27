package spend

import "strings"

// Price is one model's published per-million-token rates: the input (uncached
// prompt) and output (generated) dollar figures warden multiplies billed tokens
// by. It is the pricing half of cost governance — the spend store records REAL
// billed tokens (parse.go), this layer turns them into dollars.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// Cost prices an input/output token pair at this model's rates, in dollars.
func (p Price) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.InputPerMTok/1_000_000 +
		float64(outputTokens)*p.OutputPerMTok/1_000_000
}

// Opus input/output rates ($/MTok) — the same figures internal/savings prices
// its ledger at (savings.PricePerMTok / OutputPricePerMTok), kept in sync here so
// the spend report and the savings report can never disagree on what Claude costs.
// Opus is also the conservative default for an unrecognized model: a budget cap is
// a floor a buyer can trust, so an unknown model is priced at the most expensive
// tier rather than under-counted.
const (
	opusInputPerMTok  = 5.0
	opusOutputPerMTok = 25.0
)

// modelPrices maps a normalized model family (opus/sonnet/haiku/fable) to its
// rates. The keys are the short aliases warden already uses; PriceFor folds a
// full model id (claude-opus-4-8, us.anthropic.claude-sonnet-…) down to one of
// these families before looking it up. Rates are $/MTok as of 2026-06.
var modelPrices = map[string]Price{
	"opus":   {InputPerMTok: opusInputPerMTok, OutputPerMTok: opusOutputPerMTok},
	"sonnet": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
	"haiku":  {InputPerMTok: 0.8, OutputPerMTok: 4.0},
	"fable":  {InputPerMTok: opusInputPerMTok, OutputPerMTok: opusOutputPerMTok}, // Fable bills at the Opus tier
}

// defaultPrice is what an unrecognized (or empty) model is priced at — the Opus
// tier, deliberately the dearest, so a missing/odd model id can only over-state
// spend, never silently under-charge a budget gate.
var defaultPrice = modelPrices["opus"]

// PriceFor returns the rate table entry for a model id or alias. It is forgiving:
// it lower-cases, drops any provider prefix (the segment before the last '.', so
// "us.anthropic.claude-opus-4" → "claude-opus-4"), and matches on the family
// substring (opus/sonnet/haiku/fable) so both the short alias and the full
// versioned id resolve. Anything unrecognized falls back to the Opus default.
func PriceFor(model string) Price {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "."); i >= 0 {
		m = m[i+1:]
	}
	for _, fam := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(m, fam) {
			return modelPrices[fam]
		}
	}
	return defaultPrice
}

// Cost prices a billed input/output token pair for the given model in dollars —
// the one entry point callers use to turn measured tokens into money.
func Cost(model string, inputTokens, outputTokens int) float64 {
	return PriceFor(model).Cost(inputTokens, outputTokens)
}
