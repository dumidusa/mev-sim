package store

import (
	"database/sql"
	"fmt"
	"math/big"
)

type Pool struct {
	Address, Factory, Token0, Token1 string
	FeeBps                           int64
	CreatedBlock, CreatedTxIndex     uint64
	CreatedLogIndex                  uint64
	PairIndex                        uint64
}

type Seed struct {
	Pool               string
	Block              uint64 // reserves are the state at END of this block
	Reserve0, Reserve1 *big.Int
	Source             string // "eth_call" (OBSERVED) | "creation" (zero at pair creation)
}

type EventKey struct {
	Block, LogIndex, TxIndex uint64
	TxHash, Pool             string
}

type Sync struct {
	EventKey
	Reserve0, Reserve1 *big.Int
}

type Swap struct {
	EventKey
	Sender, Recipient                            string
	Amount0In, Amount1In, Amount0Out, Amount1Out *big.Int
}

type Block struct {
	Number, Timestamp, GasLimit, GasUsed, TxCount uint64
	Hash, ParentHash, Miner                       string
	BaseFee                                       *big.Int
}

type Transaction struct {
	Block, Index, Nonce, Type, GasLimit, Status, GasUsed uint64
	Hash, From, To, Selector                             string
	Value, GasPrice, MaxFee, MaxPriority                 *big.Int
	EffectiveGasPrice                                    *big.Int
}

func dec(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("bad decimal %q in db", s)
	}
	return v, nil
}

func (s *Store) Pools() (map[string]Pool, error) {
	rows, err := s.DB.Query(`SELECT address,factory,token0,token1,fee_bps,created_block,created_tx_index,created_log_index,pair_index FROM pools`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.Address, &p.Factory, &p.Token0, &p.Token1, &p.FeeBps, &p.CreatedBlock, &p.CreatedTxIndex, &p.CreatedLogIndex, &p.PairIndex); err != nil {
			return nil, err
		}
		out[p.Address] = p
	}
	return out, rows.Err()
}

func (s *Store) Seeds() (map[string]Seed, error) {
	rows, err := s.DB.Query(`SELECT pool,block_number,reserve0,reserve1,source FROM v2_seed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Seed{}
	for rows.Next() {
		var sd Seed
		var a, b string
		if err := rows.Scan(&sd.Pool, &sd.Block, &a, &b, &sd.Source); err != nil {
			return nil, err
		}
		if sd.Reserve0, err = dec(a); err != nil {
			return nil, err
		}
		if sd.Reserve1, err = dec(b); err != nil {
			return nil, err
		}
		out[sd.Pool] = sd
	}
	return out, rows.Err()
}

// PoolsWithEvents returns pools that emitted a Sync in [from, to].
func (s *Store) PoolsWithEvents(from, to uint64) ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT pool FROM v2_sync WHERE block_number BETWEEN ? AND ? ORDER BY pool`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Event is either a Sync or a Swap, in chain order.
type Event struct {
	Sync *Sync
	Swap *Swap
}

func (e Event) Key() EventKey {
	if e.Sync != nil {
		return e.Sync.EventKey
	}
	return e.Swap.EventKey
}

// Events streams Sync and Swap events in [from, to] ordered by (block, logIndex).
func (s *Store) Events(from, to uint64, fn func(Event) error) error {
	rows, err := s.DB.Query(`
	  SELECT 0, block_number, log_index, tx_index, tx_hash, pool, reserve0, reserve1, '', '', '', '', '', ''
	    FROM v2_sync WHERE block_number BETWEEN ?1 AND ?2
	  UNION ALL
	  SELECT 1, block_number, log_index, tx_index, tx_hash, pool, '', '', sender, recipient,
	         amount0_in, amount1_in, amount0_out, amount1_out
	    FROM v2_swap WHERE block_number BETWEEN ?1 AND ?2
	  ORDER BY 2, 3`, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind int
		var k EventKey
		var r0, r1, snd, rcp, a, b, c, d string
		if err := rows.Scan(&kind, &k.Block, &k.LogIndex, &k.TxIndex, &k.TxHash, &k.Pool, &r0, &r1, &snd, &rcp, &a, &b, &c, &d); err != nil {
			return err
		}
		var ev Event
		if kind == 0 {
			sy := &Sync{EventKey: k}
			if sy.Reserve0, err = dec(r0); err != nil {
				return err
			}
			if sy.Reserve1, err = dec(r1); err != nil {
				return err
			}
			ev.Sync = sy
		} else {
			sw := &Swap{EventKey: k, Sender: snd, Recipient: rcp}
			for _, x := range []struct {
				dst **big.Int
				s   string
			}{{&sw.Amount0In, a}, {&sw.Amount1In, b}, {&sw.Amount0Out, c}, {&sw.Amount1Out, d}} {
				if *x.dst, err = dec(x.s); err != nil {
					return err
				}
			}
			ev.Swap = sw
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Blocks streams stored block headers in [from, to] ordered by number.
func (s *Store) Blocks(from, to uint64, fn func(Block) error) error {
	rows, err := s.DB.Query(`SELECT number,hash,parent_hash,timestamp,base_fee_wei,gas_limit,gas_used,tx_count,miner
	  FROM blocks WHERE number BETWEEN ? AND ? ORDER BY number`, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b Block
		var bf sql.NullString
		if err := rows.Scan(&b.Number, &b.Hash, &b.ParentHash, &b.Timestamp, &bf, &b.GasLimit, &b.GasUsed, &b.TxCount, &b.Miner); err != nil {
			return err
		}
		if bf.Valid {
			if b.BaseFee, err = dec(bf.String); err != nil {
				return err
			}
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return rows.Err()
}
