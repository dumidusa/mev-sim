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
}

func New() *Tracker {
	return &Tracker{Stage: Detected, History: []Transition{{To: Detected}}}
}

// Advance moves exactly one stage forward; skipping stages is a bug.
func (t *Tracker) Advance(to Stage, detail string) error {
	if t.Stage == Rejected {
		return fmt.Errorf("cannot advance a rejected candidate")
	}
	for i, s := range order {
		if s == t.Stage && i+1 < len(order) && order[i+1] == to {
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
