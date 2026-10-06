package ingest

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/store"
)

type bigInt = big.Int

func zero() *big.Int { return new(big.Int) }

// touching returns, per block, tx index -> tx hash for txs that emitted a
// tracked Sync/Swap. Used to cross-check event tx hashes against blocks.
func (in *Ingestor) touching(from, to uint64) (map[uint64]map[uint64]string, error) {
	rows, err := in.st.DB.Query(`
	  SELECT block_number, tx_index, tx_hash FROM v2_sync WHERE block_number BETWEEN ?1 AND ?2
	  UNION SELECT block_number, tx_index, tx_hash FROM v2_swap WHERE block_number BETWEEN ?1 AND ?2`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]map[uint64]string{}
	for rows.Next() {
		var b, i uint64
		var h string
		if err := rows.Scan(&b, &i, &h); err != nil {
			return nil, err
		}
		if out[b] == nil {
			out[b] = map[uint64]string{}
		}
		if old, ok := out[b][i]; ok && old != h {
			return nil, fmt.Errorf("block %d tx %d: two hashes %s / %s in events", b, i, old, h)
		}
		out[b][i] = h
	}
	return out, rows.Err()
}

type fetched struct {
	block    *eth.Block
	receipts []eth.Receipt
	err      error
}

func (in *Ingestor) blocks(ctx context.Context) error {
	from, err := resumeFrom(in.st, "blocks", in.cfg.Start)
	if err != nil {
		return err
	}
	total := 0
	last := time.Now()
	for lo := from; lo <= in.cfg.End; lo += in.cfg.BlockChunk {
		hi := lo + in.cfg.BlockChunk - 1
		if hi > in.cfg.End {
			hi = in.cfg.End
		}
		touch, err := in.touching(lo, hi)
		if err != nil {
			return err
		}
		res := make([]fetched, hi-lo+1)
		var wg sync.WaitGroup
		sem := make(chan struct{}, in.cfg.Workers)
		for n := lo; n <= hi; n++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(n uint64) {
				defer wg.Done()
				defer func() { <-sem }()
				f := &res[n-lo]
				if f.block, f.err = in.chain.BlockByNumber(ctx, n); f.err != nil {
					return
				}
				if in.cfg.TxMode == "all" || len(touch[n]) > 0 {
					f.receipts, f.err = in.chain.BlockReceipts(ctx, n)
				}
			}(n)
		}
		wg.Wait()
		err = in.st.Write(func(tx store.Tx) error {
			for i := range res {
				f := res[i]
				n := lo + uint64(i)
				if f.err != nil {
					return fmt.Errorf("block %d: %w", n, f.err)
				}
				if err := in.storeBlock(tx, f, touch[n]); err != nil {
					return fmt.Errorf("block %d: %w", n, err)
				}
				total++
			}
			return tx.SetProgress("blocks", hi)
		})
		if err != nil {
			return err
		}
		if time.Since(last) > 10*time.Second || hi == in.cfg.End {
			last = time.Now()
			in.cfg.Logf("   blocks: %d / %d (%.1f%%) saved", hi, in.cfg.End,
				float64(hi-in.cfg.Start+1)/float64(in.cfg.End-in.cfg.Start+1)*100)
		}
	}
	in.cfg.Logf("   blocks stored=%d", total)
	return nil
}

func (in *Ingestor) storeBlock(tx store.Tx, f fetched, touch map[uint64]string) error {
	b := f.block
	if err := tx.InsertBlock(store.Block{Number: b.Number, Hash: b.Hash, ParentHash: b.ParentHash,
		Timestamp: b.Timestamp, BaseFee: b.BaseFee, GasLimit: b.GasLimit, GasUsed: b.GasUsed,
		TxCount: uint64(len(b.Txs)), Miner: b.Miner}); err != nil {
		return err
	}
	// Every event's tx must exist at that index with that hash.
	for idx, h := range touch {
		if idx >= uint64(len(b.Txs)) || b.Txs[idx].Hash != h {
			return fmt.Errorf("event tx %s at index %d not found in block body (reorg or bad data)", h, idx)
		}
	}
	if f.receipts == nil {
		return nil
	}
	if len(f.receipts) != len(b.Txs) {
		return fmt.Errorf("%d receipts for %d txs", len(f.receipts), len(b.Txs))
	}
	for i, t := range b.Txs {
		r := f.receipts[i]
		if r.TxHash != t.Hash || t.Index != uint64(i) {
			return fmt.Errorf("receipt/tx order mismatch at %d", i)
		}
		if in.cfg.TxMode == "touching" {
			if _, ok := touch[uint64(i)]; !ok {
				continue
			}
		}
		if r.EffectiveGasPrice == nil {
			return fmt.Errorf("tx %s: receipt has no effectiveGasPrice", t.Hash)
		}
		if err := tx.InsertTransaction(store.Transaction{Block: b.Number, Index: t.Index, Hash: t.Hash,
			From: t.From, To: t.To, Nonce: t.Nonce, Type: t.Type, Value: t.Value, GasLimit: t.GasLimit,
			GasPrice: t.GasPrice, MaxFee: t.MaxFee, MaxPriority: t.MaxPriority, Selector: t.Selector,
			Status: r.Status, GasUsed: r.GasUsed, EffectiveGasPrice: r.EffectiveGasPrice}); err != nil {
			return err
		}
	}
	return nil
}
