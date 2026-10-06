// Package ethtest is a deterministic fake Ethereum node for tests.
//
// It generates a chain of blocks where Uniswap-V2-style pairs are created,
// minted, swapped (single and multi-hop), synced after donations, plus
// noise txs and an untracked "fork" pool. It serves real JSON-RPC over HTTP
// (eth_chainId, eth_blockNumber, eth_getBlockByNumber, eth_getBlockReceipts,
// eth_getLogs, eth_call getReserves at any block = archive behaviour).
//
// It tests PLUMBING (encoding, ordering, persistence, reconstruction).
// The AMM math itself was validated against Uniswap's own vectors in Phase 1.
package ethtest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/keccak"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

const topicTransfer = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

type Factory struct {
	Address string
	FeeBps  int64 // fee the fake pools REALLY charge
}

type GenConfig struct {
	Seed          int64
	ChainID       uint64
	FirstBlock    uint64
	NumBlocks     int
	Factories     []Factory
	PoolsEarly    int // created in the first blocks (before a typical ingest start)
	PoolsMid      int // created in the middle of the chain
	MaxSwapsBlock int
	UnknownPool   bool
}

func DefaultConfig() GenConfig {
	return GenConfig{
		Seed: 7, ChainID: 1, FirstBlock: 1000, NumBlocks: 120,
		Factories: []Factory{
			{Address: addr(0xF1), FeeBps: 30},
			{Address: addr(0xF2), FeeBps: 30},
		},
		PoolsEarly: 6, PoolsMid: 2, MaxSwapsBlock: 4, UnknownPool: true,
	}
}

type pool struct {
	addr, t0, t1 string
	fee          int64
	r0, r1       *big.Int
}

type resPoint struct {
	block  uint64
	r0, r1 *big.Int
}

type Chain struct {
	cfg      GenConfig
	blocks   []*eth.Block
	receipts map[uint64][]eth.Receipt
	logs     []eth.Log
	history  map[string][]resPoint // pool -> end-of-block reserves
	created  map[string]uint64     // pool -> creation block

	mu sync.Mutex
	// Limits that mimic providers.
	MaxLogSpan    uint64
	MaxLogResults int
	// Fault injection: pool -> block where eth_call returns r0+1.
	WrongReserves map[string]uint64
	Calls         map[string]int
}

func addr(n uint64) string {
	b := make([]byte, 20)
	binary.BigEndian.PutUint64(b[12:], n)
	return eth.Hex(b)
}

func hash(parts ...any) string {
	h := keccak.Sum256([]byte(fmt.Sprint(parts...)))
	return eth.Hex(h[:])
}

func Generate(cfg GenConfig) *Chain {
	rng := rand.New(rand.NewSource(cfg.Seed))
	c := &Chain{cfg: cfg, receipts: map[uint64][]eth.Receipt{}, history: map[string][]resPoint{},
		created: map[string]uint64{}, WrongReserves: map[string]uint64{}, Calls: map[string]int{}}
	tokens := []string{addr(0xA1), addr(0xA2), addr(0xA3), addr(0xA4), addr(0xA5)}
	var pools []*pool
	nextPool := uint64(0xB000)
	pairCount := map[string]uint64{}
	parent := hash("genesis", cfg.Seed)

	createAt := map[uint64]int{}
	for i := 0; i < cfg.PoolsEarly; i++ {
		createAt[cfg.FirstBlock+uint64(i%3)]++
	}
	for i := 0; i < cfg.PoolsMid; i++ {
		createAt[cfg.FirstBlock+uint64(cfg.NumBlocks/2)]++
	}
	var unknown *pool
	if cfg.UnknownPool {
		unknown = &pool{addr: addr(0xDEAD), t0: tokens[0], t1: tokens[1], fee: 30,
			r0: e18(1000), r1: e18(2_000_000)}
	}

	for bi := 0; bi < cfg.NumBlocks; bi++ {
		num := cfg.FirstBlock + uint64(bi)
		bh := hash("block", cfg.Seed, num)
		base := big.NewInt((10 + rng.Int63n(40)) * 1e9)
		blk := &eth.Block{Number: num, Hash: bh, ParentHash: parent, Timestamp: 1_700_000_000 + 12*num,
			BaseFee: base, GasLimit: 30_000_000, Miner: addr(0xC0FFEE)}
		var rcpts []eth.Receipt
		logIdx := uint64(0)

		newTx := func() (*eth.Tx, *eth.Receipt) {
			idx := uint64(len(blk.Txs))
			prio := big.NewInt(rng.Int63n(3e9))
			maxFee := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), prio)
			tx := eth.Tx{Hash: hash("tx", cfg.Seed, num, idx), Index: idx, From: addr(0xE000 + uint64(rng.Intn(50))),
				To: addr(0x7777), Nonce: uint64(rng.Intn(1000)), Type: 2, Value: new(big.Int),
				GasLimit: 500_000, GasPrice: new(big.Int).Add(base, prio), MaxFee: maxFee, MaxPriority: prio,
				Selector: "0x38ed1739"}
			blk.Txs = append(blk.Txs, tx)
			rcpts = append(rcpts, eth.Receipt{TxHash: tx.Hash, TxIndex: idx, Status: 1,
				GasUsed: uint64(21_000 + rng.Intn(200_000)), EffectiveGasPrice: new(big.Int).Add(base, prio)})
			return &blk.Txs[len(blk.Txs)-1], &rcpts[len(rcpts)-1]
		}
		emit := func(r *eth.Receipt, address string, topics []string, data []byte) {
			l := eth.Log{Address: address, Topics: topics, Data: data, BlockNumber: num, BlockHash: bh,
				TxHash: r.TxHash, TxIndex: r.TxIndex, LogIndex: logIdx}
			logIdx++
			r.Logs = append(r.Logs, l)
		}
		transfer := func(r *eth.Receipt, token string, amt *big.Int) {
			emit(r, token, []string{topicTransfer, uniswapv2.AddrTopic(addr(1)), uniswapv2.AddrTopic(addr(2))}, uniswapv2.EncodeSync(amt, big.NewInt(0))[:32])
		}
		sync := func(r *eth.Receipt, p *pool) {
			emit(r, p.addr, []string{uniswapv2.TopicSync}, uniswapv2.EncodeSync(p.r0, p.r1))
		}
		swap := func(r *eth.Receipt, p *pool, zeroForOne bool, under bool) {
			rin, rout := p.r0, p.r1
			if !zeroForOne {
				rin, rout = p.r1, p.r0
			}
			in := new(big.Int).Quo(new(big.Int).Mul(rin, big.NewInt(1+rng.Int63n(300))), big.NewInt(10000))
			out, err := amm.GetAmountOut(in, rin, rout, p.fee)
			if err != nil || out.Sign() == 0 {
				return
			}
			if under && out.Cmp(big.NewInt(1000)) > 0 {
				out.Sub(out, big.NewInt(1+rng.Int63n(999)))
			}
			tin, tout := p.t0, p.t1
			if !zeroForOne {
				tin, tout = p.t1, p.t0
			}
			transfer(r, tin, in)
			transfer(r, tout, out)
			z := new(big.Int)
			var a0i, a1i, a0o, a1o *big.Int
			if zeroForOne {
				p.r0 = new(big.Int).Add(p.r0, in)
				p.r1 = new(big.Int).Sub(p.r1, out)
				a0i, a1i, a0o, a1o = in, z, z, out
			} else {
				p.r1 = new(big.Int).Add(p.r1, in)
				p.r0 = new(big.Int).Sub(p.r0, out)
				a0i, a1i, a0o, a1o = z, in, out, z
			}
			sync(r, p) // contract order: _update (Sync) then emit Swap
			emit(r, p.addr, []string{uniswapv2.TopicSwap, uniswapv2.AddrTopic(addr(0x7777)), uniswapv2.AddrTopic(addr(0xE001))},
				uniswapv2.EncodeSwap(a0i, a1i, a0o, a1o))
		}

		// 1. pair creations (+ initial mint in a separate tx)
		for k := 0; k < createAt[num]; k++ {
			f := cfg.Factories[(len(pools))%len(cfg.Factories)]
			i, j := rng.Intn(len(tokens)), rng.Intn(len(tokens)-1)
			if j >= i {
				j++
			}
			t0, t1 := tokens[i], tokens[j]
			if t0 > t1 {
				t0, t1 = t1, t0
			}
			p := &pool{addr: addr(nextPool), t0: t0, t1: t1, fee: f.FeeBps, r0: new(big.Int), r1: new(big.Int)}
			nextPool++
			pairCount[f.Address]++
			_, r := newTx()
			emit(r, f.Address, []string{uniswapv2.TopicPairCreated, uniswapv2.AddrTopic(t0), uniswapv2.AddrTopic(t1)},
				uniswapv2.EncodePairCreated(p.addr, pairCount[f.Address]))
			c.created[p.addr] = num
			pools = append(pools, p)
			_, r = newTx()
			p.r0 = e18(int64(100 + rng.Intn(10_000)))
			p.r1 = e18(int64(100 + rng.Intn(10_000)))
			transfer(r, t0, p.r0)
			transfer(r, t1, p.r1)
			sync(r, p)
		}
		// 2. noise txs (no pool events)
		for k := rng.Intn(3); k > 0; k-- {
			_, r := newTx()
			if rng.Intn(2) == 0 {
				transfer(r, tokens[rng.Intn(len(tokens))], big.NewInt(rng.Int63n(1e18)))
			}
		}
		// 3. swaps: single-hop, multi-hop, donation+sync
		live := []*pool{}
		for _, p := range pools {
			if p.r0.Sign() > 0 {
				live = append(live, p)
			}
		}
		for k := rng.Intn(cfg.MaxSwapsBlock + 1); k > 0 && len(live) > 0; k-- {
			_, r := newTx()
			switch rng.Intn(10) {
			case 0: // donation followed by sync(): reserves jump without a Swap
				p := live[rng.Intn(len(live))]
				p.r0 = new(big.Int).Add(p.r0, big.NewInt(rng.Int63n(1e15)+1))
				transfer(r, p.t0, big.NewInt(1))
				sync(r, p)
			case 1, 2: // multi-hop in one tx
				for h := 0; h < 2; h++ {
					swap(r, live[rng.Intn(len(live))], rng.Intn(2) == 0, false)
				}
			default:
				swap(r, live[rng.Intn(len(live))], rng.Intn(2) == 0, rng.Intn(4) == 0)
			}
		}
		if unknown != nil && rng.Intn(3) == 0 {
			_, r := newTx()
			swap(r, unknown, true, false)
		}

		for _, rc := range rcpts {
			blk.GasUsed += rc.GasUsed
			c.logs = append(c.logs, rc.Logs...)
		}
		c.blocks = append(c.blocks, blk)
		c.receipts[num] = rcpts
		for _, p := range pools {
			h := c.history[p.addr]
			if len(h) == 0 || h[len(h)-1].r0.Cmp(p.r0) != 0 || h[len(h)-1].r1.Cmp(p.r1) != 0 {
				c.history[p.addr] = append(h, resPoint{num, new(big.Int).Set(p.r0), new(big.Int).Set(p.r1)})
			}
		}
		parent = bh
	}
	return c
}

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func (c *Chain) Head() uint64 { return c.blocks[len(c.blocks)-1].Number }

// PoolAddresses returns generated pool addresses (sorted), excluding the unknown pool.
func (c *Chain) PoolAddresses() []string {
	var out []string
	for a := range c.created {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (c *Chain) CreatedAt(pool string) uint64 { return c.created[pool] }

// ReservesAt returns end-of-block reserves (what eth_call at block returns).
func (c *Chain) ReservesAt(pool string, block uint64) (*big.Int, *big.Int, bool) {
	cb, ok := c.created[pool]
	if !ok || block < cb {
		return nil, nil, false
	}
	r0, r1 := new(big.Int), new(big.Int)
	for _, p := range c.history[pool] {
		if p.block > block {
			break
		}
		r0, r1 = p.r0, p.r1
	}
	return new(big.Int).Set(r0), new(big.Int).Set(r1), true
}

// ---- JSON-RPC server -----------------------------------------------------

func (c *Chain) Server() *httptest.Server {
	return httptest.NewServer(c.Handler())
}

func (c *Chain) Handler() http.Handler { return http.HandlerFunc(c.serve) }

type rpcReq struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (c *Chain) serve(w http.ResponseWriter, r *http.Request) {
	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	c.mu.Lock()
	c.Calls[req.Method]++
	c.mu.Unlock()
	res, rerr := c.dispatch(req)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = res
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (c *Chain) block(n uint64) *eth.Block {
	if n < c.cfg.FirstBlock || n > c.Head() {
		return nil
	}
	return c.blocks[n-c.cfg.FirstBlock]
}

func bigHex(b *big.Int) any {
	if b == nil {
		return nil
	}
	return "0x" + b.Text(16)
}

func logJSON(l eth.Log) map[string]any {
	return map[string]any{"address": l.Address, "topics": l.Topics, "data": eth.Hex(l.Data),
		"blockNumber": eth.Qty(l.BlockNumber), "blockHash": l.BlockHash, "transactionHash": l.TxHash,
		"transactionIndex": eth.Qty(l.TxIndex), "logIndex": eth.Qty(l.LogIndex), "removed": false}
}

func (c *Chain) dispatch(req rpcReq) (any, *eth.RPCError) {
	str := func(i int) string {
		var s string
		if i < len(req.Params) {
			json.Unmarshal(req.Params[i], &s)
		}
		return s
	}
	qty := func(s string) uint64 { v, _ := eth.ParseQty(s); return v }
	switch req.Method {
	case "eth_chainId":
		return eth.Qty(c.cfg.ChainID), nil
	case "eth_blockNumber":
		return eth.Qty(c.Head()), nil
	case "eth_getBlockByNumber":
		b := c.block(qty(str(0)))
		if b == nil {
			return nil, nil
		}
		txs := []any{}
		for _, t := range b.Txs {
			txs = append(txs, map[string]any{"hash": t.Hash, "transactionIndex": eth.Qty(t.Index), "from": t.From,
				"to": t.To, "nonce": eth.Qty(t.Nonce), "type": eth.Qty(t.Type), "value": bigHex(t.Value),
				"gas": eth.Qty(t.GasLimit), "gasPrice": bigHex(t.GasPrice), "maxFeePerGas": bigHex(t.MaxFee),
				"maxPriorityFeePerGas": bigHex(t.MaxPriority), "input": t.Selector + "0000"})
		}
		return map[string]any{"number": eth.Qty(b.Number), "hash": b.Hash, "parentHash": b.ParentHash,
			"timestamp": eth.Qty(b.Timestamp), "baseFeePerGas": bigHex(b.BaseFee), "gasLimit": eth.Qty(b.GasLimit),
			"gasUsed": eth.Qty(b.GasUsed), "miner": b.Miner, "transactions": txs}, nil
	case "eth_getBlockReceipts":
		n := qty(str(0))
		if c.block(n) == nil {
			return nil, nil
		}
		out := []any{}
		for _, r := range c.receipts[n] {
			ls := []any{}
			for _, l := range r.Logs {
				ls = append(ls, logJSON(l))
			}
			out = append(out, map[string]any{"transactionHash": r.TxHash, "transactionIndex": eth.Qty(r.TxIndex),
				"status": eth.Qty(r.Status), "gasUsed": eth.Qty(r.GasUsed),
				"effectiveGasPrice": bigHex(r.EffectiveGasPrice), "logs": ls})
		}
		return out, nil
	case "eth_getLogs":
		return c.getLogs(req)
	case "eth_call":
		var call struct{ To, Data string }
		json.Unmarshal(req.Params[0], &call)
		n := qty(str(1))
		to := strings.ToLower(call.To)
		if call.Data != uniswapv2.SelGetReserves {
			return nil, &eth.RPCError{Code: -32000, Message: "execution reverted"}
		}
		r0, r1, ok := c.ReservesAt(to, n)
		if !ok {
			return "0x", nil // no code at that block -> empty return
		}
		if wb, bad := c.WrongReserves[to]; bad && wb == n {
			r0 = new(big.Int).Add(r0, big.NewInt(1))
		}
		return eth.Hex(uniswapv2.EncodeGetReserves(r0, r1, 0)), nil
	}
	return nil, &eth.RPCError{Code: -32601, Message: "method not found: " + req.Method}
}

func (c *Chain) getLogs(req rpcReq) (any, *eth.RPCError) {
	var f struct {
		FromBlock string          `json:"fromBlock"`
		ToBlock   string          `json:"toBlock"`
		Address   json.RawMessage `json:"address"`
		Topics    []json.RawMessage
	}
	json.Unmarshal(req.Params[0], &f)
	from, _ := eth.ParseQty(f.FromBlock)
	to, _ := eth.ParseQty(f.ToBlock)
	if c.MaxLogSpan > 0 && to-from+1 > c.MaxLogSpan {
		return nil, &eth.RPCError{Code: -32005, Message: fmt.Sprintf("query exceeds max block range %d", c.MaxLogSpan)}
	}
	addrs := map[string]bool{}
	var one string
	var many []string
	if json.Unmarshal(f.Address, &one) == nil && one != "" {
		addrs[strings.ToLower(one)] = true
	} else if json.Unmarshal(f.Address, &many) == nil {
		for _, a := range many {
			addrs[strings.ToLower(a)] = true
		}
	}
	topicSets := make([]map[string]bool, len(f.Topics))
	for i, raw := range f.Topics {
		var s string
		var arr []string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			topicSets[i] = map[string]bool{strings.ToLower(s): true}
		} else if json.Unmarshal(raw, &arr) == nil && arr != nil {
			topicSets[i] = map[string]bool{}
			for _, t := range arr {
				topicSets[i][strings.ToLower(t)] = true
			}
		}
	}
	out := []any{}
	for _, l := range c.logs {
		if l.BlockNumber < from || l.BlockNumber > to {
			continue
		}
		if len(addrs) > 0 && !addrs[l.Address] {
			continue
		}
		match := true
		for i, set := range topicSets {
			if set == nil {
				continue
			}
			if i >= len(l.Topics) || !set[l.Topics[i]] {
				match = false
				break
			}
		}
		if match {
			out = append(out, logJSON(l))
		}
	}
	if c.MaxLogResults > 0 && len(out) > c.MaxLogResults {
		return nil, &eth.RPCError{Code: -32005, Message: fmt.Sprintf("query returned more than %d results", c.MaxLogResults)}
	}
	return out, nil
}
