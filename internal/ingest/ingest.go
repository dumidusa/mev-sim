// Package ingest pulls historical Uniswap V2 data from an Ethereum node
// into the store. Five stages, each checkpointed and idempotent:
//
//	S1 chain check   chain id, finality guard
//	S2 discovery     PairCreated from each factory (deploy block → end)
//	S3 events        Sync + Swap logs by topic (start → end), tracked vs unknown
//	S4 seed          getReserves() at start−1 for active pools (archive)
//	S5 blocks        headers + touching (or all) txs with receipts
package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

type Factory struct {
	Name        string
	Address     string // lowercase
	FeeBps      int64
	DeployBlock uint64
}

// MainnetFactories: verify addresses and deploy blocks on Etherscan before
// relying on them; they are configuration, not facts the code checks.
var MainnetFactories = []Factory{
	{"uniswap_v2", "0x5c69bee701ef814a2b6a3edd4b1652cb9cc5aa6f", 30, 10000835},
	{"sushiswap", "0xc0aee478e3658e2610c5f7a4a2e1777ce9e4f2ac", 30, 10794229},
}

type Config struct {
	Start, End    uint64
	Factories     []Factory
	ChainID       uint64 // expected; 0 = don't check
	Confirmations uint64 // refuse End > head - Confirmations
	LogSpan       uint64 // initial getLogs span
	MaxLogSpan    uint64
	BlockChunk    uint64 // blocks per commit in S5
	TxMode        string // "touching" | "all"
	SeedMode      string // "active" | "all"
	Workers       int
	Logf          func(string, ...any)
}

func (c *Config) defaults() {
	if c.LogSpan == 0 {
		c.LogSpan = 2000
	}
	if c.MaxLogSpan == 0 {
		c.MaxLogSpan = 100_000
	}
	if c.BlockChunk == 0 {
		c.BlockChunk = 100
	}
	if c.TxMode == "" {
		c.TxMode = "touching"
	}
	if c.SeedMode == "" {
		c.SeedMode = "active"
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

type Ingestor struct {
	chain eth.Chain
	st    *store.Store
	cfg   Config
}

func New(chain eth.Chain, st *store.Store, cfg Config) (*Ingestor, error) {
	cfg.defaults()
	if cfg.Start == 0 || cfg.End < cfg.Start {
		return nil, fmt.Errorf("bad range %d..%d", cfg.Start, cfg.End)
	}
	if cfg.TxMode != "touching" && cfg.TxMode != "all" {
		return nil, fmt.Errorf("tx-mode must be touching|all")
	}
	if cfg.SeedMode != "active" && cfg.SeedMode != "all" {
		return nil, fmt.Errorf("seed must be active|all")
	}
	return &Ingestor{chain, st, cfg}, nil
}

func (in *Ingestor) Run(ctx context.Context) error {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"S1 chain check", in.checkChain},
		{"S2 pool discovery", in.discover},
		{"S3 Sync/Swap events", in.events},
		{"S4 seed reserves", in.seed},
		{"S5 blocks + txs", in.blocks},
	}
	for _, s := range steps {
		in.cfg.Logf("== %s", s.name)
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// ---- S1 ------------------------------------------------------------------

func (in *Ingestor) checkChain(ctx context.Context) error {
	id, err := in.chain.ChainID(ctx)
	if err != nil {
		return err
	}
	if in.cfg.ChainID != 0 && id != in.cfg.ChainID {
		return fmt.Errorf("chain id %d, expected %d", id, in.cfg.ChainID)
	}
	head, err := in.chain.BlockNumber(ctx)
	if err != nil {
		return err
	}
	if in.cfg.End+in.cfg.Confirmations > head {
		return fmt.Errorf("end %d not final: head %d, need %d confirmations", in.cfg.End, head, in.cfg.Confirmations)
	}
	// A DB belongs to one chain and one start block (seeds are taken at start−1).
	for k, v := range map[string]string{"chain_id": strconv.FormatUint(id, 10), "start_block": strconv.FormatUint(in.cfg.Start, 10)} {
		old, ok, err := in.st.Meta(k)
		if err != nil {
			return err
		}
		if ok && old != v {
			return fmt.Errorf("db has %s=%s, run asks %s (use a new db)", k, old, v)
		}
		if err := in.st.SetMeta(k, v); err != nil {
			return err
		}
	}
	return in.st.Write(func(tx store.Tx) error {
		for _, f := range in.cfg.Factories {
			if err := tx.InsertFactory(f.Address, f.Name, f.FeeBps, f.DeployBlock); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- adaptive log fetch ---------------------------------------------------

// fetchLogs walks [from, to], halving the span on provider limit errors and
// doubling it after successes. commit is called once per successful range,
// in order, so it can write data and checkpoint atomically.
func (in *Ingestor) fetchLogs(ctx context.Context, f eth.LogFilter, from, to uint64,
	commit func(from, to uint64, logs []eth.Log) error) error {
	span := in.cfg.LogSpan
	for cur := from; cur <= to; {
		hi := cur + span - 1
		if hi > to || hi < cur {
			hi = to
		}
		f.FromBlock, f.ToBlock = cur, hi
		logs, err := in.chain.GetLogs(ctx, f)
		if eth.IsLogLimit(err) {
			if hi == cur {
				return fmt.Errorf("block %d alone exceeds provider log limit: %w", cur, err)
			}
			span = (hi - cur + 1) / 2
			continue
		}
		if err != nil {
			return err
		}
		if err := commit(cur, hi, logs); err != nil {
			return err
		}
		cur = hi + 1
		if span < in.cfg.MaxLogSpan {
			span *= 2
		}
	}
	return nil
}

func resumeFrom(st *store.Store, stream string, start uint64) (uint64, error) {
	p, ok, err := st.Progress(stream)
	if err != nil || !ok || p+1 < start {
		return start, err
	}
	return p + 1, nil
}

// ---- S2 ------------------------------------------------------------------

func (in *Ingestor) discover(ctx context.Context) error {
	for _, fac := range in.cfg.Factories {
		stream := "pools:" + fac.Address
		from, err := resumeFrom(in.st, stream, fac.DeployBlock)
		if err != nil {
			return err
		}
		if from > in.cfg.End {
			continue
		}
		n := 0
		err = in.fetchLogs(ctx, eth.LogFilter{Addresses: []string{fac.Address}, Topics: [][]string{{uniswapv2.TopicPairCreated}}},
			from, in.cfg.End, func(lo, hi uint64, logs []eth.Log) error {
				return in.st.Write(func(tx store.Tx) error {
					for _, l := range logs {
						pc, err := uniswapv2.DecodePairCreated(l)
						if err != nil {
							return fmt.Errorf("block %d log %d: %w", l.BlockNumber, l.LogIndex, err)
						}
						if err := tx.InsertPool(store.Pool{Address: pc.Pair, Factory: fac.Address, Token0: pc.Token0,
							Token1: pc.Token1, FeeBps: fac.FeeBps, CreatedBlock: l.BlockNumber,
							CreatedTxIndex: l.TxIndex, CreatedLogIndex: l.LogIndex, PairIndex: pc.PairIndex}); err != nil {
							return err
						}
						n++
					}
					return tx.SetProgress(stream, hi)
				})
			})
		if err != nil {
			return err
		}
		in.cfg.Logf("   %s: %d new pools", fac.Name, n)
	}
	return nil
}

// ---- S3 ------------------------------------------------------------------

type EventStats struct{ Syncs, Swaps, Unknown int }

func (in *Ingestor) events(ctx context.Context) error {
	pools, err := in.st.Pools()
	if err != nil {
		return err
	}
	from, err := resumeFrom(in.st, "events", in.cfg.Start)
	if err != nil || from > in.cfg.End {
		return err
	}
	var stats EventStats
	err = in.fetchLogs(ctx, eth.LogFilter{Topics: [][]string{{uniswapv2.TopicSync, uniswapv2.TopicSwap}}},
		from, in.cfg.End, func(lo, hi uint64, logs []eth.Log) error {
			unknown := map[[2]string]struct {
				first uint64
				n     int
			}{}
			err := in.st.Write(func(tx store.Tx) error {
				for _, l := range logs {
					p, tracked := pools[l.Address]
					if !tracked {
						k := [2]string{l.Address, l.Topics[0]}
						u := unknown[k]
						if u.n == 0 || l.BlockNumber < u.first {
							u.first = l.BlockNumber
						}
						u.n++
						unknown[k] = u
						continue
					}
					// An event before the pool's creation log means corrupt data.
					if l.BlockNumber < p.CreatedBlock || (l.BlockNumber == p.CreatedBlock && l.LogIndex < p.CreatedLogIndex) {
						return fmt.Errorf("pool %s event at %d#%d before creation %d#%d", p.Address,
							l.BlockNumber, l.LogIndex, p.CreatedBlock, p.CreatedLogIndex)
					}
					key := store.EventKey{Block: l.BlockNumber, LogIndex: l.LogIndex, TxIndex: l.TxIndex, TxHash: l.TxHash, Pool: l.Address}
					switch l.Topics[0] {
					case uniswapv2.TopicSync:
						s, err := uniswapv2.DecodeSync(l)
						if err != nil {
							return fmt.Errorf("%s: %w", l.TxHash, err)
						}
						if err := tx.InsertSync(store.Sync{EventKey: key, Reserve0: s.Reserve0, Reserve1: s.Reserve1}); err != nil {
							return err
						}
						stats.Syncs++
					case uniswapv2.TopicSwap:
						s, err := uniswapv2.DecodeSwap(l)
						if err != nil {
							return fmt.Errorf("%s: %w", l.TxHash, err)
						}
						if err := tx.InsertSwap(store.Swap{EventKey: key, Sender: s.Sender, Recipient: s.To,
							Amount0In: s.Amount0In, Amount1In: s.Amount1In, Amount0Out: s.Amount0Out, Amount1Out: s.Amount1Out}); err != nil {
							return err
						}
						stats.Swaps++
					}
				}
				keys := make([][2]string, 0, len(unknown))
				for k := range unknown {
					keys = append(keys, k)
				}
				sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
				for _, k := range keys {
					if err := tx.BumpUnknown(k[0], k[1], unknown[k].first, unknown[k].n); err != nil {
						return err
					}
					stats.Unknown += unknown[k].n
				}
				return tx.SetProgress("events", hi)
			})
			return err
		})
	in.cfg.Logf("   syncs=%d swaps=%d untracked-emitter logs=%d", stats.Syncs, stats.Swaps, stats.Unknown)
	return err
}

// ---- S4 ------------------------------------------------------------------

func (in *Ingestor) seed(ctx context.Context) error {
	pools, err := in.st.Pools()
	if err != nil {
		return err
	}
	seeds, err := in.st.Seeds()
	if err != nil {
		return err
	}
	var targets []string
	if in.cfg.SeedMode == "all" {
		for a := range pools {
			targets = append(targets, a)
		}
	} else if targets, err = in.st.PoolsWithEvents(in.cfg.Start, in.cfg.End); err != nil {
		return err
	}
	sort.Strings(targets)
	at := in.cfg.Start - 1

	type result struct {
		pool   string
		r0, r1 *bigInt
		src    string
		err    error
	}
	var need []string
	created := 0
	err = in.st.Write(func(tx store.Tx) error {
		for _, a := range targets {
			if _, done := seeds[a]; done {
				continue
			}
			p, ok := pools[a]
			if !ok {
				return fmt.Errorf("pool %s has events but is not registered", a)
			}
			if p.CreatedBlock > at {
				// Created inside the range: reserves are exactly zero at creation.
				if err := tx.InsertSeed(a, at, zero(), zero(), "creation"); err != nil {
					return err
				}
				created++
				continue
			}
			need = append(need, a)
		}
		return nil
	})
	if err != nil {
		return err
	}
	results := make([]result, len(need))
	var wg sync.WaitGroup
	sem := make(chan struct{}, in.cfg.Workers)
	for i, a := range need {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, a string) {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := in.chain.CallAt(ctx, a, uniswapv2.GetReservesCall(), at)
			if err != nil {
				results[i] = result{pool: a, err: fmt.Errorf("getReserves(%s)@%d: %w (archive node required)", a, at, err)}
				return
			}
			r0, r1, err := uniswapv2.DecodeGetReserves(out)
			results[i] = result{pool: a, r0: r0, r1: r1, src: "eth_call", err: err}
		}(i, a)
	}
	wg.Wait()
	var errs []error
	err = in.st.Write(func(tx store.Tx) error {
		for _, r := range results {
			if r.err != nil {
				errs = append(errs, r.err)
				continue
			}
			if err := tx.InsertSeed(r.pool, at, r.r0, r.r1, r.src); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	in.cfg.Logf("   seeded via eth_call=%d, created-in-range=%d, failed=%d", len(need)-len(errs), created, len(errs))
	return errors.Join(errs...) // seeds that succeeded are kept; rerun retries the rest
}
