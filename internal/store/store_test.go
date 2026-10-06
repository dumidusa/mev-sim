package store

import (
	"math/big"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIdempotentInsertsAndHash(t *testing.T) {
	s := open(t)
	write := func() {
		err := s.Write(func(tx Tx) error {
			if err := tx.InsertSync(Sync{EventKey{10, 3, 1, "0xh", "0xp"}, big.NewInt(5), big.NewInt(6)}); err != nil {
				return err
			}
			return tx.SetProgress("events", 10)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	write()
	h1, _ := s.Hash()
	write() // same data again
	h2, _ := s.Hash()
	if h1 != h2 {
		t.Fatal("re-insert changed the data hash")
	}
	c, _ := s.Counts()
	if c["v2_sync"] != 1 {
		t.Fatalf("duplicate rows: %d", c["v2_sync"])
	}
	if p, ok, _ := s.Progress("events"); !ok || p != 10 {
		t.Fatal("progress not saved")
	}
}

func TestRollbackLeavesNoCheckpoint(t *testing.T) {
	s := open(t)
	_ = s.Write(func(tx Tx) error {
		tx.SetProgress("events", 99)
		return errFake
	})
	if _, ok, _ := s.Progress("events"); ok {
		t.Fatal("checkpoint committed despite failure")
	}
}

var errFake = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestEventsOrderedAcrossTables(t *testing.T) {
	s := open(t)
	s.Write(func(tx Tx) error {
		tx.InsertSwap(Swap{EventKey: EventKey{5, 2, 0, "0xa", "0xp"}, Amount0In: big.NewInt(1), Amount1In: big.NewInt(0), Amount0Out: big.NewInt(0), Amount1Out: big.NewInt(1)})
		tx.InsertSync(Sync{EventKey{5, 1, 0, "0xa", "0xp"}, big.NewInt(1), big.NewInt(1)})
		tx.InsertSync(Sync{EventKey{4, 9, 3, "0xb", "0xp"}, big.NewInt(1), big.NewInt(1)})
		return nil
	})
	var got []uint64
	s.Events(0, 10, func(e Event) error { k := e.Key(); got = append(got, k.Block*100+k.LogIndex); return nil })
	want := []uint64{409, 501, 502}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v want %v", got, want)
		}
	}
}

// Amounts above 2^64 must survive the round trip exactly.
func TestBigAmountsExact(t *testing.T) {
	s := open(t)
	v, _ := new(big.Int).SetString("5192296858534827628530496329220095", 10) // 2^112-1
	s.Write(func(tx Tx) error { return tx.InsertSync(Sync{EventKey{1, 0, 0, "0x", "0xp"}, v, v}) })
	s.Events(0, 1, func(e Event) error {
		if e.Sync.Reserve0.Cmp(v) != 0 {
			t.Fatalf("got %s", e.Sync.Reserve0)
		}
		return nil
	})
}
