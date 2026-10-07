package optimize

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"testing"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/route"
)

type hopSpec struct{ rin, rout float64 }

// composedOptimum: chain of CP swaps = one virtual CP pool (exact algebra):
//
//	Ra' = Ra·rin/(rin + γ·Rb),  Rb' = γ·Rb·rout/(rin + γ·Rb)
//	x*  = (√(γ·Ra·Rb) − Ra)/γ
func composedOptimum(h []hopSpec, g float64) float64 {
	Ra, Rb := h[0].rin, h[0].rout
	for _, n := range h[1:] {
		d := n.rin + g*Rb
		Ra, Rb = Ra*n.rin/d, g*Rb*n.rout/d
	}
	return (math.Sqrt(g*Ra*Rb) - Ra) / g
}

func cycle(t *testing.T, rs [][2]*big.Int) (route.State, route.Route) {
	toks := []amm.Address{"t0", "t1", "t2", "t3", "t4"}
	st := route.State{}
	var r route.Route
	for i, x := range rs {
		in, out := toks[i], toks[(i+1)%len(rs)]
		p, err := amm.NewV2Pool(amm.Address(fmt.Sprint("p", i)), in, out, x[0], x[1], 30, "uniswap_v2")
		if err != nil {
			t.Fatal(err)
		}
		st[p.Address()] = p
		r.Hops = append(r.Hops, route.H(p.Address(), in, out))
	}
	return st, r
}

// 3- and 4-hop cycles: the integer search must land on the analytic optimum.
func TestMultiHopMatchesClosedForm(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	found := 0
	for trial := 0; trial < 400 && found < 40; trial++ {
		hops := 3 + rng.Intn(2)
		var rs [][2]*big.Int
		var hs []hopSpec
		for i := 0; i < hops; i++ {
			a := (1 + rng.Float64()*9) * 1e21
			b := a * (0.9 + rng.Float64()*0.25)
			ai, _ := new(big.Float).SetFloat64(a).Int(nil)
			bi, _ := new(big.Float).SetFloat64(b).Int(nil)
			rs = append(rs, [2]*big.Int{ai, bi})
			fa, _ := new(big.Float).SetInt(ai).Float64()
			fb, _ := new(big.Float).SetInt(bi).Float64()
			hs = append(hs, hopSpec{fa, fb})
		}
		want := composedOptimum(hs, 0.997)
		st, r := cycle(t, rs)
		above, _, _, _ := route.Screen(st, r)
		if want <= 1e12 {
			if above && want < -1e12 {
				t.Fatalf("screen says profitable, closed form says not")
			}
			continue
		}
		if !above {
			t.Fatalf("closed form optimum %.3e but screen rejected", want)
		}
		f := profitFn(st, r)
		best, _ := Maximize(f, big.NewInt(1), new(big.Int).Mul(rs[0][0], big.NewInt(4)))
		got, _ := new(big.Float).SetInt(best.L).Float64()
		if rel := math.Abs(got-want) / want; rel > 1e-6 {
			t.Fatalf("%d hops: L*=%.6e closed form %.6e rel %.2e", hops, got, want, rel)
		}
		found++
	}
	if found < 10 {
		t.Fatalf("only %d profitable multi-hop cases tested", found)
	}
}

// Tiny reserves: try EVERY integer input and compare with Maximize.
//
// Finding (kept as a permanent test): integer floors make the profit curve
// slightly jagged, so ternary search can stop up to (hops) base units below
// the true integer maximum. Measured: 290/300 exact, 10/300 off by 1 unit.
// Bound enforced: gap <= hop count. 1 unit = 1e-18 WETH -> economically nil,
// but documented as the optimiser's exactness guarantee.
func TestMaximizeEqualsExhaustiveIntegerSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	checked := 0
	gaps := map[int64]int{}
	defer func() { t.Logf("gap histogram (units): %v", gaps) }()
	for trial := 0; trial < 80; trial++ {
		hops := 2 + rng.Intn(3)
		var rs [][2]*big.Int
		for i := 0; i < hops; i++ {
			a := 1000 + rng.Int63n(8000)
			b := a * (85 + rng.Int63n(40)) / 100
			rs = append(rs, [2]*big.Int{big.NewInt(a), big.NewInt(b)})
		}
		st, r := cycle(t, rs)
		f := profitFn(st, r)
		hi := big.NewInt(25000)
		var bestBrute *big.Int
		for x := int64(1); x <= hi.Int64(); x++ {
			v, err := f(big.NewInt(x))
			if err == nil && (bestBrute == nil || v.Cmp(bestBrute) > 0) {
				bestBrute = v
			}
		}
		got, _ := Maximize(f, big.NewInt(1), hi)
		if bestBrute == nil {
			if got.Profit != nil {
				t.Fatal("found a profit where none exists")
			}
			continue
		}
		if got.Profit == nil {
			t.Fatalf("trial %d: nothing found, exhaustive %s", trial, bestBrute)
		}
		gap := new(big.Int).Sub(bestBrute, got.Profit).Int64()
		if gap < 0 {
			t.Fatal("Maximize above exhaustive maximum: impossible")
		}
		gaps[gap]++
		if gap > int64(hops) {
			t.Fatalf("trial %d (%d hops): gap %d units > hop count", trial, hops, gap)
		}
		checked++
	}
	t.Logf("%d cases checked exhaustively", checked)
}

// Soundness of the exact screen: whenever it rejects, no integer input in a
// wide range is profitable.
func TestScreenRejectionIsSound(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	rejected := 0
	for trial := 0; trial < 300; trial++ {
		hops := 2 + rng.Intn(3)
		var rs [][2]*big.Int
		for i := 0; i < hops; i++ {
			a := 1000 + rng.Int63n(50000)
			b := a * (80 + rng.Int63n(45)) / 100
			rs = append(rs, [2]*big.Int{big.NewInt(a), big.NewInt(b)})
		}
		st, r := cycle(t, rs)
		above, _, _, err := route.Screen(st, r)
		if err != nil {
			t.Fatal(err)
		}
		if above {
			continue
		}
		rejected++
		f := profitFn(st, r)
		for x := int64(1); x <= 100000; x += 1 + x/50 {
			if v, err := f(big.NewInt(x)); err == nil && v.Sign() > 0 {
				t.Fatalf("screen rejected but L=%d gives profit %s", x, v)
			}
		}
	}
	if rejected < 50 {
		t.Fatalf("only %d rejections tested", rejected)
	}
}

func TestScreenRejectsNonV2(t *testing.T) {
	st := route.State{}
	r := route.Route{Hops: []route.Hop{route.H("x", "a", "b")}}
	if _, _, _, err := route.Screen(st, r); err == nil {
		t.Fatal("missing pool accepted")
	}
}
