// Package optimize finds the profit-maximising loan size L* and samples
// the profit curve Profit(L).
//
// Why ternary search is valid: for any AMM whose marginal price worsens as
// you trade more (V2 constant product, V3 concentrated liquidity), output(L)
// is concave, so Profit(L) = output(L) - L - fee(L) is unimodal.
// Integer floor rounding can create 1-wei plateaus; the search narrows to a
// small window and then scans it exhaustively.
package optimize

import (
	"math"
	"math/big"
)

// ProfitFn returns profit for loan size L, or an error if L is infeasible.
type ProfitFn func(L *big.Int) (*big.Int, error)

type Point struct {
	L      *big.Int
	Profit *big.Int // nil when Err != nil
	Err    error
}

func better(a, b *big.Int) bool { // nil == -infinity
	if a == nil {
		return false
	}
	return b == nil || a.Cmp(b) > 0
}

// Maximize searches integer L in [lo, hi]. Returns best point and #evaluations.
func Maximize(f ProfitFn, lo, hi *big.Int) (Point, int) {
	evals := 0
	eval := func(x *big.Int) *big.Int {
		evals++
		v, err := f(x)
		if err != nil {
			return nil
		}
		return v
	}
	l, h := new(big.Int).Set(lo), new(big.Int).Set(hi)
	three, window := big.NewInt(3), big.NewInt(64)
	for new(big.Int).Sub(h, l).Cmp(window) > 0 {
		third := new(big.Int).Quo(new(big.Int).Sub(h, l), three)
		m1 := new(big.Int).Add(l, third)
		m2 := new(big.Int).Sub(h, third)
		if better(eval(m2), eval(m1)) {
			l = m1
		} else {
			h = m2
		}
	}
	best := Point{}
	for x := new(big.Int).Set(l); x.Cmp(h) <= 0; x.Add(x, big.NewInt(1)) {
		v, err := f(x)
		evals++
		if err == nil && better(v, best.Profit) {
			best = Point{new(big.Int).Set(x), v, nil}
		}
	}
	return best, evals
}

// Curve samples Profit(L) at n log-spaced points in [lo, hi] (lo >= 1).
func Curve(f ProfitFn, lo, hi *big.Int, n int) []Point {
	if lo.Sign() <= 0 {
		lo = big.NewInt(1)
	}
	lf, _ := new(big.Float).SetInt(lo).Float64()
	hf, _ := new(big.Float).SetInt(hi).Float64()
	pts := make([]Point, 0, n)
	for i := 0; i < n; i++ {
		x := lf * math.Pow(hf/lf, float64(i)/float64(n-1))
		L, _ := new(big.Float).SetFloat64(x).Int(nil)
		if i == n-1 {
			L = new(big.Int).Set(hi)
		}
		v, err := f(L)
		pts = append(pts, Point{L, v, err})
	}
	return pts
}
