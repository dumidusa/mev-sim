package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/dumidu/mev-sim/internal/store"
)

func str(b *big.Int) any {
	if b == nil {
		return nil
	}
	return b.String()
}

func ratStr(r *big.Rat) any {
	if r == nil {
		return nil
	}
	return r.FloatString(6)
}

func s(b *big.Int) string {
	if b == nil {
		return "-"
	}
	return b.String()
}

// ConfigJSON is the exact configuration of a run (every assumption).
func (r *Result) ConfigJSON() string {
	c := r.Cfg
	hops := map[string]uint64{}
	for h, g := range c.Gas.PerHops {
		hops[fmt.Sprint(h)] = g
	}
	m := map[string]any{
		"start": c.Start, "end": c.End, "anchor": c.Anchor, "max_hops": c.MaxHops, "max_routes": c.MaxRoutes,
		"flash_provider": c.Flash.Name, "flash_fee_num": s(c.Flash.FeeNum), "flash_fee_den": s(c.Flash.FeeDen),
		"flash_liquidity_wei": s(c.Flash.MaxLiquidity), "flash_source": c.FlashSource,
		"gas_used_by_hops": hops, "gas_source": c.Gas.Source, "gas_note": c.Gas.Note,
		"priority_fee_wei": s(c.Gas.PriorityWei), "priority_source": c.Gas.PrioritySrc, "priority_note": c.Gas.PriorityNote,
		"builder_share_bps": c.BuilderShareBps, "min_profit_wei": s(c.MinProfitWei), "curve_points": c.CurvePoints,
		"usd_pool": c.USDPool, "usd_token": c.USDToken, "usd_decimals": c.USDDecimals,
		"competition": NotModeled,
	}
	b, _ := json.Marshal(m) // map keys are sorted -> deterministic
	return string(b)
}

// ResultsHash fingerprints every result row in a canonical order. Same data +
// same config + same code must give the same hash.
func (r *Result) ResultsHash() string {
	h := sha256.New()
	w := func(parts ...any) {
		for _, p := range parts {
			fmt.Fprint(h, p, "\x1f")
		}
		fmt.Fprint(h, "\x1e")
	}
	for _, c := range r.Cands {
		w("c", c.ID, r.Set.Routes[c.Route].RouteID, c.Point, c.TriggerTx, c.Fingerprint, c.Stage, c.Reason,
			s(c.L), s(c.Out), s(c.Gross), s(c.FlashFee), c.GasUsed, s(c.BaseFee), s(c.Priority), s(c.GasCost),
			s(c.Builder), s(c.Net), ratStr(c.NetUSD), c.Episode)
	}
	for _, e := range r.Episodes {
		cl := "-"
		if e.Closed != nil {
			cl = e.Closed.String()
		}
		w("e", e.ID, e.Route, e.First, e.Last, cl, e.ClosedTx, e.ClosedBy, e.States, e.FirstCand, e.BestCand,
			e.FirstValidCand, e.Public, e.PublicFrom, e.Class, e.GroupGross, e.GroupValid)
	}
	for _, g := range r.Groups {
		w("g", g.ID, g.Kind, g.Point, fmt.Sprint(g.Members), g.Best, g.Cand, s(g.Value), g.Feasible, s(g.FeasibleVal))
	}
	for _, a := range r.Arbs {
		w("a", a.Arb.TxHash, a.Arb.Sub, a.ModelMatch, a.RouteMatch, s(a.RouteSimOut), a.RouteID, a.Recognized, a.Reason,
			a.ScanStage, s(a.ScanL), s(a.ScanGross), a.GasUsed, s(a.GasCostObs), fmt.Sprint(a.Captures))
	}
	for _, t := range r.Tokens {
		w("t", t.Token, t.Status, t.Exact, t.Short, t.Over, t.EvidenceTx)
	}
	keys := make([]string, 0, len(r.Metrics))
	for k := range r.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("m", k, r.Metrics[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

type RunMeta struct {
	RunID, CodeVersion, DataHash, InputsHash, ResultsHash, CreatedAt string
}

// Save writes one run atomically and returns its metadata.
func Save(st *store.Store, r *Result, codeVersion string) (RunMeta, error) {
	dataHash, err := st.Hash()
	if err != nil {
		return RunMeta{}, err
	}
	cfg := r.ConfigJSON()
	ih := sha256.Sum256([]byte(codeVersion + "|" + dataHash + "|" + cfg))
	m := RunMeta{
		CodeVersion: codeVersion, DataHash: dataHash, InputsHash: hex.EncodeToString(ih[:]),
		ResultsHash: r.ResultsHash(), CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	m.RunID = fmt.Sprintf("run-%s-%s", time.Now().UTC().Format("20060102T150405.000"), m.InputsHash[:8])
	m.RunID = strings.ReplaceAll(m.RunID, ".", "")
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	err = st.Write(func(tx store.Tx) error {
		ex := func(q string, a ...any) error { _, err := tx.Exec(q, a...); return err }
		if err := ex(`INSERT INTO scan_runs VALUES(?,?,?,?,?,?,?,?,?)`, m.RunID, m.CreatedAt, codeVersion, dataHash,
			r.Cfg.Start, r.Cfg.End, cfg, m.InputsHash, m.ResultsHash); err != nil {
			return err
		}
		for i, rt := range r.Set.Routes {
			if err := ex(`INSERT INTO scan_routes VALUES(?,?,?,?,?,?,?,?,?)`, m.RunID, i, rt.RouteID, rt.CycleID,
				rt.Forward, rt.Hops(), rt.Start, strings.Join(rt.Tokens, ","), strings.Join(rt.Pools, ",")); err != nil {
				return err
			}
		}
		for _, c := range r.Cands {
			var hops, curve []map[string]string
			for _, h := range c.Hops {
				hops = append(hops, map[string]string{"pool": h.Pool, "in_token": h.TokenIn, "out_token": h.TokenOut,
					"in": s(h.In), "out": s(h.Out), "pool_fee_in_token": s(h.PoolFee)})
			}
			for _, p := range c.Curve {
				e := ""
				if p.Err != nil {
					e = p.Err.Error()
				}
				curve = append(curve, map[string]string{"L": s(p.L), "profit": s(p.Profit), "err": e})
			}
			var ep any
			if c.Episode >= 0 {
				ep = c.Episode
			}
			if err := ex(`INSERT INTO scan_candidates VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				m.RunID, c.ID, c.Route, c.Point.Block, c.Point.After, c.TriggerTx, c.Fingerprint, string(c.Stage),
				nullable(string(c.Reason)), nullable(c.Detail), str(c.RateNum), str(c.RateDen),
				str(c.L), str(c.Out), str(c.Gross), str(c.FlashFee), c.GasUsed, str(c.BaseFee), str(c.Priority), str(c.GasCost),
				str(c.Builder), str(c.PreBuilder), str(c.Net), ratStr(c.ETHUSD), ratStr(c.NetUSD),
				c.UnverifiedTokens, ep, js(hops), js(curve), js(c.Provenance)); err != nil {
				return err
			}
		}
		for _, e := range r.Episodes {
			var cb, ca any
			if e.Closed != nil {
				cb, ca = e.Closed.Block, e.Closed.After
			}
			var fv, pf any
			if e.FirstValidCand >= 0 {
				fv = e.FirstValidCand
			}
			if e.Public {
				pf = e.PublicFrom
			}
			if err := ex(`INSERT INTO scan_episodes VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				m.RunID, e.ID, e.Route, e.First.Block, e.First.After, e.Last.Block, e.Last.After, cb, ca,
				nullable(e.ClosedTx), e.ClosedBy, e.States, e.FirstCand, e.BestCand, fv, e.Public, pf, e.Class,
				neg(e.GroupGross), neg(e.GroupValid)); err != nil {
				return err
			}
		}
		for _, g := range r.Groups {
			if err := ex(`INSERT INTO scan_groups VALUES(?,?,?,?,?,?,?,?,?,?,?)`, m.RunID, g.ID, g.Kind, g.Point.Block,
				g.Point.After, js(g.Members), g.Best, g.Cand, s(g.Value), g.Feasible, str(g.FeasibleVal)); err != nil {
				return err
			}
		}
		for k, v := range r.Metrics {
			if err := ex(`INSERT INTO scan_metrics VALUES(?,?,?)`, m.RunID, k, v); err != nil {
				return err
			}
		}
		for _, t := range r.Tokens {
			if err := ex(`INSERT INTO scan_tokens VALUES(?,?,?,?,?,?,?)`, m.RunID, t.Token, t.Status, t.Exact, t.Short,
				t.Over, nullable(t.EvidenceTx)); err != nil {
				return err
			}
		}
		for _, a := range r.Arbs {
			var hc []map[string]string
			for _, h := range a.Hops {
				hc = append(hc, map[string]string{"pool": h.Pool, "obs_in": s(h.ObsIn), "obs_out": s(h.ObsOut),
					"model_out": s(h.ModelOut), "result": h.Result})
			}
			if err := ex(`INSERT INTO scan_hist_arbs VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				m.RunID, a.Arb.TxHash, a.Arb.Sub, a.Arb.Block, a.Arb.TxIndex, len(a.Arb.Legs), a.Arb.Start,
				strings.Join(a.Arb.Pools(), ","), s(a.Arb.AmountIn), s(a.Arb.AmountOut), s(a.Arb.Gross),
				str(a.RouteSimOut), a.ModelMatch+"/"+a.RouteMatch, js(hc), nullable(a.RouteID), a.InRouteSet, a.Recognized, a.Reason,
				nullable(string(a.ScanStage)), str(a.ScanL), str(a.ScanGross), a.GasUsed, str(a.EffGasPrice),
				str(a.BaseFee), str(a.PriorityObs), str(a.GasCostObs), js(a.Captures)); err != nil {
				return err
			}
		}
		return nil
	})
	return m, err
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func neg(i int) any {
	if i < 0 {
		return nil
	}
	return i
}
