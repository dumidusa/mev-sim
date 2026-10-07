package histarb

import (
	"math/big"
	"testing"
)

func leg(pool, in, out string, a, b int64, to string) Leg {
	return Leg{Pool: pool, TokenIn: in, TokenOut: out, AmountIn: big.NewInt(a), AmountOut: big.NewInt(b), Recipient: to}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		legs []Leg
		pat  string
		arbs int
	}{
		{"single", []Leg{leg("A", "w", "u", 10, 20, "x")}, PatSingle, 0},
		{"router multi-hop", []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "d", 20, 19, "x")}, PatOpenChain, 0},
		{"unchained", []Leg{leg("A", "w", "u", 10, 20, "x"), leg("B", "d", "w", 5, 1, "x")}, PatUnchained, 0},
		{"2-hop arb", []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "w", 20, 11, "x")}, PatCycleProfit, 1},
		{"round trip loss", []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "w", 20, 9, "x")}, PatCycleLoss, 0},
		{"same pool twice", []Leg{leg("A", "w", "u", 10, 20, "A"), leg("A", "u", "w", 20, 11, "x")}, PatPoolRepeat, 0},
		{"two arbs in one tx", []Leg{
			leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "w", 20, 11, "C"),
			leg("C", "w", "d", 11, 30, "D"), leg("D", "d", "w", 30, 12, "x")}, PatCycleProfit, 2},
		{"3-hop arb", []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "d", 20, 21, "C"), leg("C", "d", "w", 21, 12, "x")}, PatCycleProfit, 1},
	}
	for _, c := range cases {
		pat, arbs := Classify(&Tx{TxHash: "0xt", Legs: c.legs})
		if pat != c.pat || len(arbs) != c.arbs {
			t.Errorf("%s: got %s/%d want %s/%d", c.name, pat, len(arbs), c.pat, c.arbs)
		}
	}
	_, arbs := Classify(&Tx{Legs: []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "d", 20, 21, "C"), leg("C", "d", "w", 21, 12, "x")}})
	a := arbs[0]
	if a.Gross.Int64() != 2 || a.Start != "w" || len(a.Pools()) != 3 || a.Tokens()[3] != "w" {
		t.Fatalf("arb fields %+v", a)
	}
}

func TestTokenEvidence(t *testing.T) {
	txs := []*Tx{
		{TxHash: "0x1", Legs: []Leg{leg("A", "w", "u", 10, 20, "B"), leg("B", "u", "d", 20, 5, "x")}},   // u exact
		{TxHash: "0x2", Legs: []Leg{leg("A", "w", "f", 10, 100, "B"), leg("B", "f", "d", 99, 5, "x")}},  // f short
		{TxHash: "0x3", Legs: []Leg{leg("A", "w", "g", 10, 100, "me"), leg("B", "g", "d", 90, 5, "x")}}, // via contract: no evidence
	}
	ev := Tokens(txs)
	if ev["u"].Status != TokenVerified || ev["f"].Status != TokenFoT || ev["f"].EvidenceTx != "0x2" {
		t.Fatalf("got %+v %+v", ev["u"], ev["f"])
	}
	if _, ok := ev["g"]; ok {
		t.Fatal("transfer via contract must not count as evidence")
	}
}
