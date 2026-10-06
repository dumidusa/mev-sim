// Package v2state reconstructs exact Uniswap V2 reserves from stored Sync
// events.
//
// Why this is exact: a V2 pair changes reserves only in _update(), and
// _update() always emits Sync(reserve0, reserve1) with the NEW absolute
// values. So the reserves after any log = the last Sync up to that log.
// Replay is "overwrite", never "add".
//
// Position semantics: ReservesAt(pool, B, i) is the state a NEW transaction
// inserted just before tx i of block B would see (all Syncs from txs < i).
// ReservesEndOfBlock(pool, B) equals getReserves() called at block tag B.
package v2state

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/route"
	"github.com/dumidu/mev-sim/internal/store"
)

var (
	ErrNotCreated = errors.New("pool not created at this position")
	ErrNotSeeded  = errors.New("pool not seeded (no reserves known at range start)")
	ErrBeforeSeed = errors.New("position is before the ingested range")
)

type Source string

const (
	FromSeed Source = "seed" // getReserves at start-1 (OBSERVED) or zero at creation
	FromSync Source = "sync" // last Sync before position (RECONSTRUCTED)
)

type Reserves struct {
	R0, R1 *big.Int
	Source Source
	// Sync event the value came from (zero for seed).
	Block, LogIndex uint64
}

type Reader struct {
	st    *store.Store
	pools map[string]store.Pool
	seeds map[string]store.Seed
}

func NewReader(st *store.Store) (*Reader, error) {
	p, err := st.Pools()
	if err != nil {
		return nil, err
	}
	s, err := st.Seeds()
	if err != nil {
		return nil, err
	}
	return &Reader{st, p, s}, nil
}

func (r *Reader) Pool(addr string) (store.Pool, bool) { p, ok := r.pools[addr]; return p, ok }

// ReservesAt returns reserves just before tx txIndex of block.
func (r *Reader) ReservesAt(pool string, block, txIndex uint64) (Reserves, error) {
	p, ok := r.pools[pool]
	if !ok {
		return Reserves{}, fmt.Errorf("unknown pool %s", pool)
	}
	// The pool exists once its creation tx has executed.
	if block < p.CreatedBlock || (block == p.CreatedBlock && txIndex <= p.CreatedTxIndex) {
		return Reserves{}, ErrNotCreated
	}
	seed, ok := r.seeds[pool]
	if !ok {
		return Reserves{}, ErrNotSeeded
	}
	if block <= seed.Block {
		return Reserves{}, ErrBeforeSeed
	}
	var a, b string
	var res Reserves
	err := r.st.DB.QueryRow(`SELECT reserve0, reserve1, block_number, log_index FROM v2_sync
	  WHERE pool=? AND block_number > ? AND (block_number < ? OR (block_number = ? AND tx_index < ?))
	  ORDER BY block_number DESC, log_index DESC LIMIT 1`,
		pool, seed.Block, block, block, txIndex).Scan(&a, &b, &res.Block, &res.LogIndex)
	if err == sql.ErrNoRows {
		return Reserves{R0: new(big.Int).Set(seed.Reserve0), R1: new(big.Int).Set(seed.Reserve1), Source: FromSeed}, nil
	}
	if err != nil {
		return Reserves{}, err
	}
	res.R0, _ = new(big.Int).SetString(a, 10)
	res.R1, _ = new(big.Int).SetString(b, 10)
	res.Source = FromSync
	return res, nil
}

// ReservesEndOfBlock = state after every tx of block (= eth_call at block).
func (r *Reader) ReservesEndOfBlock(pool string, block uint64) (Reserves, error) {
	return r.ReservesAt(pool, block+1, 0)
}

// Snapshot builds a Phase-1 route.State (exact V2 pools) at a position.
// Pools that do not exist or are not seeded there are returned in skipped,
// never silently given zero reserves.
func (r *Reader) Snapshot(pools []string, block, txIndex uint64) (route.State, map[string]error, error) {
	st := route.State{}
	skipped := map[string]error{}
	for _, a := range pools {
		res, err := r.ReservesAt(a, block, txIndex)
		if errors.Is(err, ErrNotCreated) || errors.Is(err, ErrNotSeeded) || errors.Is(err, ErrBeforeSeed) {
			skipped[a] = err
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		p := r.pools[a]
		pool, err := amm.NewV2Pool(amm.Address(a), amm.Address(p.Token0), amm.Address(p.Token1), res.R0, res.R1, p.FeeBps, p.Factory)
		if err != nil {
			return nil, nil, err
		}
		st[amm.Address(a)] = pool
	}
	return st, skipped, nil
}

// ---- streaming replayer --------------------------------------------------

// Replayer applies events in chain order to in-memory state. It is the
// fast path for scanning every position in a range (Phase 3).
type Replayer struct {
	Pools map[string]store.Pool
	State map[string]*Reserves // only seeded pools
	last  store.EventKey
	any   bool
}

func NewReplayer(st *store.Store) (*Replayer, error) {
	pools, err := st.Pools()
	if err != nil {
		return nil, err
	}
	seeds, err := st.Seeds()
	if err != nil {
		return nil, err
	}
	rp := &Replayer{Pools: pools, State: map[string]*Reserves{}}
	for a, s := range seeds {
		rp.State[a] = &Reserves{R0: new(big.Int).Set(s.Reserve0), R1: new(big.Int).Set(s.Reserve1), Source: FromSeed}
	}
	return rp, nil
}

// Apply consumes one event. Events must arrive in strictly increasing
// (block, logIndex) order.
func (rp *Replayer) Apply(ev store.Event) (before Reserves, err error) {
	k := ev.Key()
	if rp.any && (k.Block < rp.last.Block || (k.Block == rp.last.Block && k.LogIndex <= rp.last.LogIndex)) {
		return before, fmt.Errorf("event %d#%d out of order after %d#%d", k.Block, k.LogIndex, rp.last.Block, rp.last.LogIndex)
	}
	rp.last, rp.any = k, true
	cur, ok := rp.State[k.Pool]
	if !ok {
		return before, fmt.Errorf("%w: %s at %d", ErrNotSeeded, k.Pool, k.Block)
	}
	before = *cur
	if ev.Sync != nil {
		rp.State[k.Pool] = &Reserves{R0: ev.Sync.Reserve0, R1: ev.Sync.Reserve1, Source: FromSync, Block: k.Block, LogIndex: k.LogIndex}
	}
	return before, nil
}
