// Package lifecycle is the opportunity state machine. Nothing is silently
// dropped: every candidate ends either at an accepted stage or REJECTED with
// a machine-readable reason.
package lifecycle

import "fmt"

type Stage string

const (
	Detected          Stage = "DETECTED"
	Simulated         Stage = "SIMULATED"
	Profitable        Stage = "PROFITABLE"
	EconomicallyValid Stage = "ECONOMICALLY_VALID"
	ExecutionFeasible Stage = "EXECUTION_FEASIBLE"
	CompetitionFilter Stage = "COMPETITION_FILTER"
	HistoricalResult  Stage = "HISTORICAL_RESULT"
	Rejected          Stage = "REJECTED"
)

var order = []Stage{Detected, Simulated, Profitable, EconomicallyValid, ExecutionFeasible, CompetitionFilter, HistoricalResult}

// Scanner stages (Phase 3). Each one removes exactly one cost or check.
const (
	GrossProfitable     Stage = "GROSS_PROFITABLE"
	FlashLoanProfitable Stage = "FLASH_LOAN_PROFITABLE"
	GasProfitable       Stage = "GAS_PROFITABLE"
)

var ScanOrder = []Stage{Detected, Simulated, GrossProfitable, FlashLoanProfitable, GasProfitable, EconomicallyValid, ExecutionFeasible}

// Rank is the position of s in ScanOrder (-1 if absent).
func Rank(s Stage) int {
	for i, x := range ScanOrder {
		if x == s {
			return i
		}
	}
	return -1
}

type Reason string

const (
	RouteNotExecutable          Reason = "route_not_executable"
	UnsupportedProtocol         Reason = "unsupported_protocol"
	SimulationFailed            Reason = "simulation_failed"
	NoGrossProfit               Reason = "no_gross_profit"
	FlashLoanFeeExceedsProfit   Reason = "flash_loan_fee_exceeds_profit"
	NegativeAfterGas            Reason = "negative_after_gas"
	NegativeAfterBuilderPayment Reason = "negative_after_builder_payment"
	BelowMinProfit              Reason = "below_min_profit"
	InsufficientFlashLiquidity  Reason = "insufficient_flash_liquidity"
	FlashLiquidityUnknown       Reason = "flash_liquidity_unknown"
	ExceedsBlockGasLimit        Reason = "exceeds_block_gas_limit"
	NoPriceSource               Reason = "no_price_source"

	// scanner reasons
	UnsupportedTokenBehavior      Reason = "unsupported_token_behavior"
	NumericalConstraint           Reason = "numerical_constraint"
	GasExceedsProfit              Reason = "gas_exceeds_profit"
	GasModelUnavailable           Reason = "gas_model_unavailable"
	BuilderPaymentExceedsProfit   Reason = "builder_payment_exceeds_profit"
	InsufficientLiquidity         Reason = "insufficient_liquidity"
	FlashFeeExceedsProfit         Reason = "flash_fee_exceeds_profit"
	CompetitionAdjustmentNegative Reason = "competition_adjustment_negative" // reserved: competition not modeled
)

type Transition struct {
	To     Stage
	Reason Reason
	Detail string
}

type Tracker struct {
	Stage   Stage
	Reason  Reason
	History []Transition
	order   []Stage
}

func New() *Tracker { return NewWithOrder(order) }

// NewWithOrder starts a tracker at ord[0] that may only advance along ord.
func NewWithOrder(ord []Stage) *Tracker {
	return &Tracker{Stage: ord[0], History: []Transition{{To: ord[0]}}, order: ord}
}

// Advance moves exactly one stage forward; skipping stages is a bug.
func (t *Tracker) Advance(to Stage, detail string) error {
	if t.Stage == Rejected {
		return fmt.Errorf("cannot advance a rejected candidate")
	}
	for i, s := range t.order {
		if s == t.Stage && i+1 < len(t.order) && t.order[i+1] == to {
			t.Stage = to
			t.History = append(t.History, Transition{To: to, Detail: detail})
			return nil
		}
	}
	return fmt.Errorf("illegal transition %s -> %s", t.Stage, to)
}

// Reject records the stage where the candidate failed and why.
func (t *Tracker) Reject(r Reason, detail string) {
	t.History = append(t.History, Transition{To: Rejected, Reason: r, Detail: fmt.Sprintf("at %s: %s", t.Stage, detail)})
	t.Stage, t.Reason = Rejected, r
}

// LastAccepted is the furthest stage reached before rejection.
func (t *Tracker) LastAccepted() Stage {
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].To != Rejected {
			return t.History[i].To
		}
	}
	return Detected
}
