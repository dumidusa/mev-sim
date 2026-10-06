package v2state

import (
	"errors"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/dumidu/mev-sim/internal/store"
)

func TestReplayerGuards(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Write(func(tx store.Tx) error {
		tx.InsertPool(store.Pool{Address: "0xp", Factory: "0xf", Token0: "0xa", Token1: "0xb", FeeBps: 30, CreatedBlock: 1})
		return tx.InsertSeed("0xp", 9, big.NewInt(100), big.NewInt(200), "eth_call")
	})
	rp, _ := NewReplayer(st)
	s := func(b, li uint64, pool string, r0 int64) store.Event {
		return store.Event{Sync: &store.Sync{EventKey: store.EventKey{Block: b, LogIndex: li, Pool: pool}, Reserve0: big.NewInt(r0), Reserve1: big.NewInt(1)}}
	}
	before, err := rp.Apply(s(10, 5, "0xp", 150))
	if err != nil || before.R0.Int64() != 100 || before.Source != FromSeed {
		t.Fatalf("first apply: %v %v", before, err)
	}
	if rp.State["0xp"].R0.Int64() != 150 {
		t.Fatal("Sync must overwrite reserves")
	}
	if _, err := rp.Apply(s(10, 5, "0xp", 1)); err == nil {
		t.Fatal("duplicate position accepted")
	}
	if _, err := rp.Apply(s(9, 99, "0xp", 1)); err == nil {
		t.Fatal("out-of-order event accepted")
	}
	if _, err := rp.Apply(s(11, 0, "0xq", 1)); !errors.Is(err, ErrNotSeeded) {
		t.Fatalf("unseeded pool: %v", err)
	}
}
