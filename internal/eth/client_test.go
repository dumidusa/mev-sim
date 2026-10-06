package eth_test

import (
	"context"
	"testing"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

func TestClientAgainstFakeNode(t *testing.T) {
	ch := ethtest.Generate(ethtest.DefaultConfig())
	srv := ch.Server()
	defer srv.Close()
	c := eth.NewClient(srv.URL)
	c.MaxRetries = 0
	ctx := context.Background()

	if id, err := c.ChainID(ctx); err != nil || id != 1 {
		t.Fatalf("chainId %d %v", id, err)
	}
	head, _ := c.BlockNumber(ctx)
	b, err := c.BlockByNumber(ctx, head)
	if err != nil || b.Number != head || b.BaseFee == nil {
		t.Fatalf("block %+v %v", b, err)
	}
	rc, err := c.BlockReceipts(ctx, head)
	if err != nil || len(rc) != len(b.Txs) {
		t.Fatalf("receipts %d vs txs %d (%v)", len(rc), len(b.Txs), err)
	}
	for i := range rc {
		if rc[i].TxHash != b.Txs[i].Hash {
			t.Fatalf("receipt %d hash mismatch", i)
		}
	}
	logs, err := c.GetLogs(ctx, eth.LogFilter{FromBlock: 1000, ToBlock: head,
		Topics: [][]string{{uniswapv2.TopicSync, uniswapv2.TopicSwap}}})
	if err != nil || len(logs) == 0 {
		t.Fatalf("logs %d %v", len(logs), err)
	}
	ch.MaxLogSpan = 10
	_, err = c.GetLogs(ctx, eth.LogFilter{FromBlock: 1000, ToBlock: 1100})
	if !eth.IsLogLimit(err) {
		t.Fatalf("range limit not detected: %v", err)
	}
	pool := ch.PoolAddresses()[0]
	out, err := c.CallAt(ctx, pool, uniswapv2.GetReservesCall(), head)
	if err != nil {
		t.Fatal(err)
	}
	r0, r1, err := uniswapv2.DecodeGetReserves(out)
	w0, w1, _ := ch.ReservesAt(pool, head)
	if err != nil || r0.Cmp(w0) != 0 || r1.Cmp(w1) != 0 {
		t.Fatalf("getReserves mismatch")
	}
}
