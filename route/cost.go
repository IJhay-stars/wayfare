// CostDecomposition breaks the effective transfer cost into separately-reported
// components: FX loss, network fees, anchor fee, slippage, and expected
// failure cost.
//
// Currently, the verdict reports a single loss percentage against fair value.
// That number is useful but opaque. Showing the decomposition turns a single
// verdict into actionable information.
//
// Each component is computed and reported independently. Network fees and
// anchor fees are reported separately because they have different sources:
// network fees are Stellar base-fee charges per operation, while anchor fees
// are the anchor's own charge for a conversion, obtainable via SEP-38 when
// the anchor publishes an ANCHOR_QUOTE_SERVER. Expected failure cost stays
// explicitly unknown until failure history exists.
//
// Slippage is the one component a lone quote cannot establish: it is the
// loss a size added on top of the corridor's structural floor, and a floor
// is a comparison across sizes. Decompose therefore leaves it undetermined;
// Engine.Ladder computes it per rung once at least two sizes have priced,
// via LadderResult.decomposeSlippage, which documents the definition.
package route

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// CostComponent names one piece of the effective transfer cost.
type CostComponent string

const (
	CostFXLoss          CostComponent = "fx_loss"
	CostNetworkFees     CostComponent = "network_fees"
	CostAnchorFee       CostComponent = "anchor_fee"
	CostSlippage        CostComponent = "slippage"
	CostExpectedFailure CostComponent = "expected_failure"
)

// CostPart is one component of the effective transfer cost.
type CostPart struct {
	Component  CostComponent
	Amount     decimal.Decimal
	Pct        decimal.Decimal
	Determined bool
	Reason     string
}

// CostDecomposition is the full breakdown of a route's effective transfer cost.
type CostDecomposition struct {
	Parts        []CostPart
	TotalLossPct decimal.Decimal
}

// Decompose splits a priced route's effective transfer cost into components.
func Decompose(q Quote, mid decimal.Decimal) CostDecomposition {
	_ = mid // reserved: will carry the independent mid-market rate for future FX-loss-from-mid calculation
	parts := make([]CostPart, 0, 5)

	// FX loss: difference between effective rate and mid, as a percentage.
	fxLossPct := q.LossPct
	fxLossAmount := q.LossAmount
	parts = append(parts, CostPart{
		Component:  CostFXLoss,
		Amount:     fxLossAmount,
		Pct:        fxLossPct,
		Determined: true,
	})

	// Network fees: undetermined. A Stellar path payment charges a base fee per
	// operation, and a multi-hop path is more operations than a direct one,
	// but Decompose sees only a Quote and has neither the path's operation
	// count nor a currently-effective base fee. Naming the gap keeps the
	// units honest: unavailable is unknown, not a default, and small is not
	// zero.
	parts = append(parts, CostPart{
		Component:  CostNetworkFees,
		Amount:     decimal.Zero,
		Pct:        decimal.Zero,
		Determined: false,
		Reason:     "network fee not measured; determining it requires the path's operation count and the current Stellar base fee",
	})

	// Anchor fee: the anchor's own charge for a conversion, obtainable via
	// SEP-38 when the anchor publishes an ANCHOR_QUOTE_SERVER in its
	// stellar.toml. A DEX-only route has no anchor involved, and an anchor
	// that does not publish a quote server has no machine-readable rate — the
	// absence is a fact about the anchor rather than a zero fee.
	//
	// When a KindAnchorSEP38 quote is available, its FeeInBuyAsset (already
	// normalised for denomination by sep38.Quote) can be wired here.
	anchorFeeReason := "anchor fee not available; the anchor does not publish an ANCHOR_QUOTE_SERVER, so its fee cannot be obtained programmatically"
	if q.Kind == KindAnchorSEP38 {
		// TODO: extract anchor fee from a sep38.Quote when a corridor with
		// SEP-38 support is wired in. The sep38.Quote.FeeInBuyAsset field
		// already carries the converted fee.
		anchorFeeReason = "anchor fee available via SEP-38 but no corridor with a published quote server has been priced yet"
	}
	parts = append(parts, CostPart{
		Component:  CostAnchorFee,
		Amount:     decimal.Zero,
		Pct:        decimal.Zero,
		Determined: false,
		Reason:     anchorFeeReason,
	})

	// Slippage: undetermined from a lone quote, and only from a lone
	// quote. A single size cannot show what the size itself cost — the
	// comparison is across sizes, and the ladder holds the neighbours.
	// This reason is accurate about Decompose's inputs, not about the
	// system: the ladder determines slippage wherever at least two sizes
	// priced (see LadderResult.decomposeSlippage).
	parts = append(parts, CostPart{
		Component:  CostSlippage,
		Amount:     decimal.Zero,
		Pct:        decimal.Zero,
		Determined: false,
		Reason:     "single quote available; slippage requires a comparison across sizes, which the ladder computes where at least two sizes priced",
	})

	// Expected failure cost: explicitly unknown.
	parts = append(parts, CostPart{
		Component:  CostExpectedFailure,
		Amount:     decimal.Zero,
		Pct:        decimal.Zero,
		Determined: false,
		Reason:     "no failure history exists yet; runstore is collecting but has not accumulated enough observations",
	})

	return CostDecomposition{
		Parts:        parts,
		TotalLossPct: q.LossPct,
	}
}

// findPart returns the named component's part within a decomposition, or
// nil when the decomposition does not carry it.
func findPart(d *CostDecomposition, c CostComponent) *CostPart {
	for i := range d.Parts {
		if d.Parts[i].Component == c {
			return &d.Parts[i]
		}
	}
	return nil
}

// decomposeSlippage replaces the placeholder slippage part on every priced
// rung's decomposition with the ladder-measured figure, and re-scopes that
// rung's fx_loss to the structural floor so the determined components still
// partition the total rather than overlapping it.
//
// # The floor
//
// The floor is the loss at the ladder's smallest priced rung. At that size
// price impact is negligible — the trade is dust — so what remains is the
// corridor's structural cost: its spread against the reference mid, not its
// depth. This is the split the project was founded on: 25.02% loss at
// 0.1 USDC is the structural floor; 97.68% at 5000 is that floor plus
// 72.66 points of slippage.
//
// # Slippage
//
// Slippage at a priced size is the excess loss that size added on top of
// the floor: its loss percentage minus the floor's. Only measured sizes are
// used — no interpolation between rungs, no prediction at unmeasured ones.
// Floor and excess come from the same two measurements, so fx_loss plus
// slippage equals the rung's total loss exactly; the reconciliation
// tolerance is zero.
//
// The floor rung's slippage is a determined zero, not an undetermined one:
// the size was measured, and at the floor there is by definition nothing
// beneath it to compare against. Zero is a finding; unknown is the absence
// of one, and the Determined flag is what keeps the two apart.
//
// With fewer than two priced rungs there is nothing to compare against and
// slippage stays undetermined, with a reason naming the shortfall.
//
// A determined slippage can be negative when a larger size priced better
// than the floor rung. That is a measured non-monotonic curve (see
// ExecutionRateCurve.NonMonotonic), and it is reported as measured rather
// than smoothed into zero.
func (l *LadderResult) decomposeSlippage() {
	priced := make([]int, 0, len(l.Rungs))
	for i := range l.Rungs {
		if l.Rungs[i].Priced() {
			priced = append(priced, i)
		}
	}

	if len(priced) < 2 {
		// The real reason slippage is unknown here: the ladder did not
		// price enough sizes to compare, however many it attempted.
		reason := fmt.Sprintf(
			"slippage undetermined: %d of %d ladder sizes priced; "+
				"a comparison across sizes needs at least two priced rungs",
			len(priced), len(l.Rungs))
		for _, i := range priced {
			if p := findPart(&l.Rungs[i].Decomposition, CostSlippage); p != nil {
				p.Determined = false
				p.Amount = decimal.Zero
				p.Pct = decimal.Zero
				p.Reason = reason
			}
		}
		return
	}

	// Rungs are held in ascending size order, so the first priced rung is
	// the smallest priced size — the floor by definition. summarise sets
	// Floor and FloorSize from the same rung.
	floorQuote := l.Rungs[priced[0]].Result.Quotes[0]
	floorPct, floorAmount := floorQuote.LossPct, floorQuote.LossAmount

	for _, i := range priced {
		r := &l.Rungs[i]
		q := r.Result.Quotes[0]

		// fx_loss is re-scoped from the rung's whole loss to the
		// structural floor — what this corridor costs at negligible
		// price impact. Without this the determined components would
		// overlap: fx_loss would already contain the slippage now
		// reported beside it, and a reader summing parts would count
		// the depth effect twice.
		if fx := findPart(&r.Decomposition, CostFXLoss); fx != nil {
			fx.Amount = floorAmount
			fx.Pct = floorPct
			fx.Determined = true
			fx.Reason = "structural floor: the loss at the ladder's smallest priced size, where price impact is negligible"
		}

		slippage := findPart(&r.Decomposition, CostSlippage)
		if slippage == nil {
			continue
		}
		slippage.Amount = q.LossAmount.Sub(floorAmount)
		slippage.Pct = q.LossPct.Sub(floorPct)
		slippage.Determined = true
		if i == priced[0] {
			slippage.Reason = "zero by definition: this rung is the corridor's structural floor, the smallest priced size; slippage is the excess loss above the floor"
		} else {
			slippage.Reason = ""
		}
	}
}
