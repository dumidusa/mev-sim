package amm

import (
	"math/big"
	"math/rand"
	"testing"
)

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
func bi(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }

// Test vectors from Uniswap v2-core test/UniswapV2Pair.spec.ts (swapTestCases):
// [swapAmount, token0Reserve, token1Reserve, expectedOutput] in 1e18 units.
func TestGetAmountOut_UniswapV2CoreVectors(t *testing.T) {
	cases := []struct {
		in, r0, r1 int64
		want       string
	}{
		{1, 5, 10, "1662497915624478906"},
		{1, 10, 5, "453305446940074565"},
		{2, 5, 10, "2851015155847869602"},
		{2, 10, 5, "831248957812239453"},
		{1, 10, 10, "906610893880149131"},
		{1, 100, 100, "987158034397061298"},
		{1, 1000, 1000, "996006981039903216"},
	}
	for _, c := range cases {
		got, err := GetAmountOut(e18(c.in), e18(c.r0), e18(c.r1), 30)
		if err != nil {
			t.Fatal(err)
		}
		if got.Cmp(bi(c.want)) != 0 {
			t.Errorf("in=%d r=(%d,%d): got %s want %s", c.in, c.r0, c.r1, got, c.want)
		}
	}
}

// The 997/1000 form used on-chain must equal our 9970/10000 form.
func TestFeeFormEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		in := big.NewInt(rng.Int63n(1e15) + 1)
		r0 := big.NewInt(rng.Int63n(1e17) + 1)
		r1 := big.NewInt(rng.Int63n(1e17) + 1)
		ours, _ := GetAmountOut(in, r0, r1, 30)
		w := new(big.Int).Mul(in, big.NewInt(997))
		ref := new(big.Int).Mul(w, r1)
		ref.Quo(ref, new(big.Int).Add(new(big.Int).Mul(r0, big.NewInt(1000)), w))
		if ours.Cmp(ref) != 0 {
			t.Fatalf("mismatch in=%s r0=%s r1=%s ours=%s ref=%s", in, r0, r1, ours, ref)
		}
	}
}

// Property: k never decreases, and GetAmountIn(GetAmountOut(x)) <= x.
func TestInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 5000; i++ {
		r0 := new(big.Int).Mul(big.NewInt(rng.Int63n(1e9)+1000), big.NewInt(1e12))
		r1 := new(big.Int).Mul(big.NewInt(rng.Int63n(1e9)+1000), big.NewInt(1e12))
		p, _ := NewV2Pool("0xp", "0xa", "0xb", r0, r1, 30, "uniswap_v2")
		k0 := new(big.Int).Mul(r0, r1)
		in := big.NewInt(rng.Int63n(1e18) + 1)
		out, err := p.SwapExactIn("0xa", in)
		if err != nil {
			continue // tiny inputs legitimately revert with zero output
		}
		a, b := p.Reserves()
		if new(big.Int).Mul(a, b).Cmp(k0) < 0 {
			t.Fatalf("k decreased")
		}
		need, _ := GetAmountIn(out, r0, r1, 30)
		if need.Cmp(in) > 0 {
			t.Fatalf("getAmountIn(%s)=%s > amountIn %s", out, need, in)
		}
	}
}

// State mutation: a second identical swap must get LESS output.
func TestSequentialSwapsUseNewReserves(t *testing.T) {
	p, _ := NewV2Pool("0xp", "0xa", "0xb", e18(10_000), e18(20_000_000), 30, "uniswap_v2")
	first, _ := p.SwapExactIn("0xa", e18(100))
	second, _ := p.SwapExactIn("0xa", e18(100))
	if second.Cmp(first) >= 0 {
		t.Fatalf("second swap %s should be < first %s", second, first)
	}
	r0, r1 := p.Reserves()
	if r0.Cmp(e18(10_200)) != 0 {
		t.Fatalf("reserve0 = %s", r0)
	}
	if want := new(big.Int).Sub(new(big.Int).Sub(e18(20_000_000), first), second); r1.Cmp(want) != 0 {
		t.Fatalf("reserve1 = %s want %s", r1, want)
	}
}

func TestQuoteDoesNotMutate(t *testing.T) {
	p, _ := NewV2Pool("0xp", "0xa", "0xb", e18(10), e18(10), 30, "uniswap_v2")
	p.QuoteExactIn("0xa", e18(1))
	r0, r1 := p.Reserves()
	if r0.Cmp(e18(10)) != 0 || r1.Cmp(e18(10)) != 0 {
		t.Fatal("quote mutated state")
	}
}

func TestReverts(t *testing.T) {
	p, _ := NewV2Pool("0xp", "0xa", "0xb", big.NewInt(1000), big.NewInt(1000), 30, "uniswap_v2")
	if _, err := p.SwapExactIn("0xa", big.NewInt(0)); err != ErrInsufficientInputAmount {
		t.Errorf("zero input: %v", err)
	}
	if _, err := p.SwapExactIn("0xa", big.NewInt(1)); err != ErrInsufficientOutputAmount {
		t.Errorf("dust input: %v", err)
	}
	if _, err := p.SwapExactIn("0xc", big.NewInt(10)); err == nil {
		t.Errorf("unknown token accepted")
	}
	big1, _ := NewV2Pool("0xq", "0xa", "0xb", MaxUint112, big.NewInt(1e18), 30, "uniswap_v2")
	if _, err := big1.SwapExactIn("0xa", big.NewInt(1e18)); err != ErrOverflow {
		t.Errorf("overflow: %v", err)
	}
}

func BenchmarkGetAmountOut(b *testing.B) {
	in, r0, r1 := e18(1), e18(10_000), e18(20_000_000)
	for i := 0; i < b.N; i++ {
		GetAmountOut(in, r0, r1, 30)
	}
}
