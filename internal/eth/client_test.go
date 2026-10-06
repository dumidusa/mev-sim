package eth_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

func fastClient(url string) *eth.Client {
	c := eth.NewClient(url)
	c.MaxRetries = 0
	c.BaseThrottleWait, c.MaxThrottleWait = time.Millisecond, 5*time.Millisecond
	return c
}

// Both throttle styles (HTTP 429 and JSON "compute units") are waited out.
func TestThrottleIsRetried(t *testing.T) {
	ch := ethtest.Generate(ethtest.DefaultConfig())
	ch.ThrottleHTTPEvery, ch.ThrottleJSONEvery = 2, 3 // most requests refused at first
	srv := ch.Server()
	defer srv.Close()
	c := fastClient(srv.URL)
	for i := 0; i < 20; i++ {
		if _, err := c.BlockByNumber(context.Background(), 1000+uint64(i)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if c.Stats.Throttled.Load() == 0 {
		t.Fatal("no throttling observed")
	}
}

func TestThrottleGivesUpWithClearError(t *testing.T) {
	ch := ethtest.Generate(ethtest.DefaultConfig())
	ch.ThrottleHTTPEvery = 1 // refuse everything
	srv := ch.Server()
	defer srv.Close()
	c := fastClient(srv.URL)
	c.MaxThrottleRetries = 3
	_, err := c.BlockNumber(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--rps") {
		t.Fatalf("got %v", err)
	}
}

func TestRPSLimit(t *testing.T) {
	ch := ethtest.Generate(ethtest.DefaultConfig())
	srv := ch.Server()
	defer srv.Close()
	c := fastClient(srv.URL)
	c.SetRPS(50) // 20 ms apart
	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // 8 workers share one limit
		wg.Add(1)
		go func() { defer wg.Done(); c.BlockNumber(context.Background()) }()
	}
	wg.Wait()
	if d := time.Since(t0); d < 130*time.Millisecond {
		t.Fatalf("8 requests at 50 rps took %s, limiter not shared", d)
	}
}

func TestThrottleIsNotALogLimit(t *testing.T) {
	thr := &eth.RPCError{Code: 429, Message: "exceeded its compute units per second capacity"}
	if eth.IsLogLimit(thr) || !eth.IsThrottle(thr) {
		t.Fatal("throttle misclassified as log range limit")
	}
	lim := &eth.RPCError{Code: -32600, Message: "You can make eth_getLogs requests with up to a 10 block range"}
	if !eth.IsLogLimit(lim) || eth.IsThrottle(lim) {
		t.Fatal("log range limit misclassified")
	}
}
