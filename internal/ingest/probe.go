package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dumidu/mev-sim/internal/eth"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

func (in *Ingestor) factoryByAddr(a string) Factory {
	for _, f := range in.cfg.Factories {
		if f.Address == a {
			return f
		}
	}
	return Factory{}
}

// isNotPair: the contract answered, and the answer is "I am not a V2 pair"
// (revert / no code). Transport or rate-limit errors are NOT this - they abort.
func isNotPair(err error) bool {
	var re *eth.RPCError
	if !errors.As(err, &re) {
		return false
	}
	m := strings.ToLower(re.Message)
	return strings.Contains(m, "revert") || re.Code == 3
}

// probe asks each address factory(), token0(), token1() at block Start-1.
// Pools created inside the range were already registered from PairCreated,
// so anything asked here either existed before Start or is not ours.
func (in *Ingestor) probe(ctx context.Context, addrs []string) ([]store.Probe, error) {
	at := in.cfg.Start - 1
	out := make([]store.Probe, len(addrs))
	errs := make([]error, len(addrs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, in.cfg.Workers)
	for i, a := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, a string) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := store.Probe{Address: a, Block: at, Status: store.ProbeNotPair}
			vals := make([]string, 3)
			for j, sel := range []string{uniswapv2.SelFactory, uniswapv2.SelToken0, uniswapv2.SelToken1} {
				raw, err := in.chain.CallAt(ctx, a, uniswapv2.Call(sel), at)
				if isNotPair(err) {
					out[i] = pr
					return
				}
				if err != nil {
					errs[i] = fmt.Errorf("probe %s@%d: %w", a, at, err)
					return
				}
				v, err := uniswapv2.DecodeAddressReturn(raw)
				if err != nil { // empty "0x" = no code at that block, or not an address
					out[i] = pr
					return
				}
				vals[j] = v
			}
			pr.Factory, pr.Token0, pr.Token1 = vals[0], vals[1], vals[2]
			if in.factoryByAddr(pr.Factory).Address != "" {
				pr.Status = store.ProbeRegistered
			} else {
				pr.Status = store.ProbeOtherFactory
			}
			out[i] = pr
		}(i, a)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}
