package keccak

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestKnownVectors(t *testing.T) {
	cases := map[string]string{
		"":    "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
		"abc": "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45",
		// ERC-20 Transfer topic, known on every block explorer
		"Transfer(address,address,uint256)": "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
	}
	for in, want := range cases {
		h := Sum256([]byte(in))
		if got := hex.EncodeToString(h[:]); got != want {
			t.Errorf("keccak(%q) = %s want %s", in, got, want)
		}
	}
}

// Inputs that cross the 136-byte rate boundary.
func TestLongInputsStable(t *testing.T) {
	seen := map[[32]byte]bool{}
	for _, n := range []int{135, 136, 137, 271, 272, 273, 1000} {
		h := Sum256([]byte(strings.Repeat("a", n)))
		if seen[h] {
			t.Fatalf("collision at length %d", n)
		}
		seen[h] = true
	}
}
