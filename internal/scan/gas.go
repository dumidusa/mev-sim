package scan

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/dumidu/mev-sim/internal/histarb"
	"github.com/dumidu/mev-sim/internal/store"
)

// MinGasSamples: below this many observed arbitrage txs for a hop count,
// no gas estimate is produced for it (candidates stop at gas_model_unavailable).
const MinGasSamples = 3

// EstimateGas derives the gas model from ON-CHAIN arbitrage txs in the range:
// median gasUsed per hop count, median priority fee (effectiveGasPrice -
// baseFee). Label ESTIMATED: those bots may not use flash loans, and direct
// builder payments (coinbase transfers) are NOT AVAILABLE without traces.
func EstimateGas(st *store.Store, arbsByTx map[[2]uint64][]histarb.Arb) (GasModel, error) {
	gm := GasModel{PerHops: map[int]uint64{}, Source: Estimated, PrioritySrc: Estimated}
	byHops := map[int][]uint64{}
	var prios []*big.Int
	keys := make([][2]uint64, 0, len(arbsByTx))
	for k := range arbsByTx {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || (keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1])
	})
	for _, k := range keys {
		var gu uint64
		var egp, bf string
		err := st.DB.QueryRow(`SELECT t.gas_used, t.effective_gas_price, COALESCE(b.base_fee_wei,'0')
		  FROM transactions t JOIN blocks b ON b.number=t.block_number
		  WHERE t.block_number=? AND t.tx_index=? AND t.status=1`, k[0], k[1]).Scan(&gu, &egp, &bf)
		if err != nil {
			continue // tx row not stored: no observation
		}
		e, _ := new(big.Int).SetString(egp, 10)
		b, _ := new(big.Int).SetString(bf, 10)
		if e != nil && b != nil {
			prios = append(prios, new(big.Int).Sub(e, b))
		}
		if arbs := arbsByTx[k]; len(arbs) == 1 { // one clean cycle: gas belongs to it
			h := len(arbs[0].Legs)
			byHops[h] = append(byHops[h], gu)
		}
	}
	var parts []string
	hs := make([]int, 0, len(byHops))
	for h := range byHops {
		hs = append(hs, h)
	}
	sort.Ints(hs)
	for _, h := range hs {
		v := byHops[h]
		if len(v) < MinGasSamples {
			parts = append(parts, fmt.Sprintf("%d-hop: only %d obs (unavailable)", h, len(v)))
			continue
		}
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		gm.PerHops[h] = v[len(v)/2]
		parts = append(parts, fmt.Sprintf("%d-hop: median %d of %d obs", h, gm.PerHops[h], len(v)))
	}
	gm.Note = fmt.Sprintf("median gasUsed of observed on-chain V2 arbitrage txs in range (%v); flash-loan overhead not included", parts)
	if len(prios) >= MinGasSamples {
		sort.Slice(prios, func(i, j int) bool { return prios[i].Cmp(prios[j]) < 0 })
		gm.PriorityWei = prios[len(prios)/2]
		gm.PriorityNote = fmt.Sprintf("median (effectiveGasPrice - baseFee) of %d observed arbitrage txs; coinbase transfers NOT AVAILABLE", len(prios))
	} else {
		gm.PriorityWei = new(big.Int)
		gm.PrioritySrc = Assumed
		gm.PriorityNote = fmt.Sprintf("0 (only %d observed arbitrage txs; pass --priority-gwei)", len(prios))
	}
	return gm, nil
}
