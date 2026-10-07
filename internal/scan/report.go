package scan

import (
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/dumidu/mev-sim/internal/lifecycle"
)

// MinReliableDays: below this sample length every per-day / per-month figure
// is printed with a SAMPLE TOO SMALL warning.
const MinReliableDays = 7.0

const TargetUSDPerMonth = 100000

var wei = big.NewInt(1e18)

func ethS(v *big.Int) string {
	if v == nil {
		return "n/a"
	}
	return new(big.Rat).SetFrac(v, wei).FloatString(6) + " ETH"
}

func usdS(r *big.Rat) string {
	if r == nil {
		return "NOT AVAILABLE"
	}
	return "$" + r.FloatString(2)
}

func pct(a, b int) string {
	if b == 0 {
		return "  -  "
	}
	return fmt.Sprintf("%5.1f%%", 100*float64(a)/float64(b))
}

// nearest-rank percentile on ascending values
func pctl(sorted []*big.Int, p float64) *big.Int {
	if len(sorted) == 0 {
		return nil
	}
	k := int(p/100*float64(len(sorted)) + 0.999999)
	if k < 1 {
		k = 1
	}
	if k > len(sorted) {
		k = len(sorted)
	}
	return sorted[k-1]
}

func sum(v []*big.Int) *big.Int {
	t := new(big.Int)
	for _, x := range v {
		t.Add(t, x)
	}
	return t
}

func ratio(a, b *big.Int) float64 {
	if b == nil || b.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	return f
}

// Opp is one deduplicated economic opportunity (a "valid" group).
type Opp struct {
	G    *Group
	C    *Candidate
	E    *Episode
	Net  *big.Int
	Days float64
}

func (r *Result) opportunities() []Opp {
	var out []Opp
	for _, g := range r.Groups {
		if g.Kind != "valid" {
			continue
		}
		out = append(out, Opp{G: g, C: r.Cands[g.Cand], E: r.Episodes[g.Best], Net: g.Value})
	}
	return out
}

// Span returns covered seconds (block timestamps) and days.
func (r *Result) Span() (uint64, float64) {
	a, b := r.Blocks[r.Cfg.Start], r.Blocks[r.Cfg.End]
	if b.Timestamp <= a.Timestamp {
		return 0, 0
	}
	sec := b.Timestamp - a.Timestamp + 12 // + one slot: the last block's own interval
	return sec, float64(sec) / 86400
}

func Report(w io.Writer, r *Result, m RunMeta) {
	P := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	bar, sub := strings.Repeat("=", 72), strings.Repeat("-", 72)
	head := func(t string) { P(""); P(sub); P("%s", t); P(sub) }
	sec, days := r.Span()
	small := days < MinReliableDays
	warn := ""
	if small {
		warn = fmt.Sprintf("   [SAMPLE TOO SMALL: %.2f h of chain - extrapolation NOT RELIABLE]", float64(sec)/3600)
	}

	P(bar)
	P("HISTORICAL ARBITRAGE SCAN  (historically observed opportunity within the modeled universe)")
	P(bar)
	P("run           %s", m.RunID)
	P("code          %s", m.CodeVersion)
	P("data hash     %s", m.DataHash)
	P("results hash  %s", m.ResultsHash)
	P("")
	P("Blocks:                  %d -> %d (%d blocks, %.2f hours)", r.Cfg.Start, r.Cfg.End, r.Cfg.End-r.Cfg.Start+1, float64(sec)/3600)
	P("States scanned:          %d tx boundaries (+1 start state)", r.Metrics["scan_points"]-1)
	P("Pools modeled:           %d  (%d on a cycle after 2-core pruning)", r.Coverage.ModeledPools, r.Coverage.PoolsInCore)
	hops := make([]string, 0)
	for h := 2; h <= r.Cfg.MaxHops; h++ {
		hops = append(hops, fmt.Sprintf("%d-hop %d", h, r.Set.Stats.RoutesByHops[h]))
	}
	P("Routes enumerated:       %d  (%s), anchor %s", len(r.Set.Routes), strings.Join(hops, ", "), r.Cfg.Anchor)
	if r.Set.Stats.Truncated {
		P("                         !!! TRUNCATED at --max-routes: enumeration INCOMPLETE")
	}
	P("Route evaluations:       %d   (same-state skips %d, inactive %d)", r.Metrics["route_evaluations"],
		r.Metrics["route_skipped_same_state"], r.Metrics["route_skipped_inactive"])

	// ---- funnel ----
	head("FUNNEL  (each level removes exactly one thing)")
	reach := map[lifecycle.Stage]int{}
	reasons := map[string]int{}
	for _, c := range r.Cands {
		for _, s := range lifecycle.ScanOrder {
			if c.reached(s) {
				reach[s]++
			}
		}
		if c.Reason != "" {
			reasons[fmt.Sprintf("%-32s at %s", c.Reason, c.Stage)]++
		}
	}
	ev := int(r.Metrics["route_evaluations"])
	P("%-34s %9d", "route evaluations", ev)
	P("%-34s %9d   %s  (exact screen: provably unprofitable)", "  no price discrepancy", r.Metrics["no_discrepancy"], pct(int(r.Metrics["no_discrepancy"]), ev))
	labels := map[lifecycle.Stage]string{
		lifecycle.Detected: "DETECTED (discrepancy > pool fees)", lifecycle.Simulated: "SIMULATED (sized exactly)",
		lifecycle.GrossProfitable: "GROSS_PROFITABLE", lifecycle.FlashLoanProfitable: "FLASH_LOAN_PROFITABLE",
		lifecycle.GasProfitable: "GAS_PROFITABLE", lifecycle.EconomicallyValid: "ECONOMICALLY_VALID",
		lifecycle.ExecutionFeasible: "EXECUTION_FEASIBLE",
	}
	for _, s := range lifecycle.ScanOrder {
		P("%-34s %9d", labels[s], reach[s])
	}
	P("COMPETITION                        NOT MODELED")
	if len(reasons) > 0 {
		P("")
		P("rejection reasons (candidate states):")
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return reasons[keys[i]] > reasons[keys[j]] || (reasons[keys[i]] == reasons[keys[j]] && keys[i] < keys[j])
		})
		for _, k := range keys {
			P("  %-60s %7d", k, reasons[k])
		}
	}

	// ---- dedup ----
	head("DEDUPLICATION  (states -> episodes -> opportunities)")
	validEp, feasEp := 0, 0
	for _, e := range r.Episodes {
		if e.FirstValidCand >= 0 {
			validEp++
			if r.Cands[e.FirstValidCand].reached(lifecycle.ExecutionFeasible) {
				feasEp++
			}
		}
	}
	gg, vg, fg := 0, 0, 0
	for _, g := range r.Groups {
		switch {
		case g.Kind == "gross":
			gg++
		case g.Feasible:
			vg++
			fg++
		default:
			vg++
		}
	}
	P("%-44s %9d", "candidate states (DETECTED)", len(r.Cands))
	P("%-44s %9d", "episodes (route stays gross-profitable)", len(r.Episodes))
	P("%-44s %9d", "  of which reach ECONOMICALLY_VALID", validEp)
	P("%-44s %9d", "gross opportunities (groups by first state)", gg)
	P("%-44s %9d   <- primary count", "ECONOMIC OPPORTUNITIES (valid groups)", vg)
	P("%-44s %9d", "  of which EXECUTION_FEASIBLE", fg)

	opps := r.opportunities()
	vals := make([]*big.Int, len(opps))
	for i, o := range opps {
		vals[i] = o.Net
	}
	sorted := append([]*big.Int(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cmp(sorted[j]) < 0 })
	total := sum(vals)

	// ---- distribution ----
	head("PROFIT DISTRIBUTION  (net per opportunity, value at first valid state)")
	if len(sorted) == 0 {
		P("no economically valid opportunity in this sample")
	} else {
		mean := new(big.Int).Quo(total, big.NewInt(int64(len(sorted))))
		for _, x := range []struct {
			l string
			v *big.Int
		}{{"Mean", mean}, {"P25", pctl(sorted, 25)}, {"Median", pctl(sorted, 50)}, {"P75", pctl(sorted, 75)},
			{"P90", pctl(sorted, 90)}, {"P95", pctl(sorted, 95)}, {"P99", pctl(sorted, 99)}, {"P99.9", pctl(sorted, 99.9)},
			{"Maximum", sorted[len(sorted)-1]}} {
			P("%-26s %s", x.l+":", ethS(x.v))
		}
	}
	// cost breakdown over the SAME opportunity set (each opportunity's own candidate)
	g, fl, gas, bld := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
	for _, o := range opps {
		g.Add(g, o.C.Gross)
		fl.Add(fl, o.C.FlashFee)
		gas.Add(gas, o.C.GasCost)
		bld.Add(bld, o.C.Builder)
	}
	P("")
	P("Cost breakdown (same %d opportunities):", len(opps))
	P("  %-24s %s   (DEX fees + price impact already inside: EXACT)", "Gross profit:", ethS(g))
	P("  %-24s %s", "- Flash-loan fees:", ethS(fl))
	P("  %-24s %s", "- Gas:", ethS(gas))
	P("  %-24s %s", "- Builder payments:", ethS(bld))
	P("  %-24s %s   <- primary", "= Total net profit:", ethS(total))
	upper := new(big.Int)
	for _, e := range r.Episodes {
		if c := r.Cands[e.BestCand]; c.Net != nil && c.Net.Sign() > 0 {
			upper.Add(upper, c.Net)
		}
	}
	firstGross := new(big.Int)
	for _, gr := range r.Groups {
		if gr.Kind == "gross" {
			firstGross.Add(firstGross, gr.Value)
		}
	}
	P("")
	P("%-26s %s   (UPPER BOUND: best state of every episode; hindsight + overlap)", "Net upper bound:", ethS(upper))
	P("%-26s %s   (all %d gross opportunities at the state they FIRST appeared)", "Gross at first appearance:", ethS(firstGross), gg)
	usd := r.usdTotal(opps)
	P("%-26s %s", "Total net profit (USD):", usdS(usd))

	// ---- rates ----
	head("OPPORTUNITY RATE")
	nBlocks := float64(r.Cfg.End - r.Cfg.Start + 1)
	P("Opportunities/block:      %.4f", float64(len(opps))/nBlocks)
	P("Net profit/block:         %s", ethS(new(big.Int).Quo(total, big.NewInt(int64(nBlocks)))))
	if days > 0 {
		perDay := new(big.Rat).Quo(new(big.Rat).SetInt(total), new(big.Rat).SetFloat64(days))
		P("Opportunities/day:        %.1f%s", float64(len(opps))/days, warn)
		P("Net profit/day:           %s ETH%s", new(big.Rat).Quo(perDay, new(big.Rat).SetInt(wei)).FloatString(6), warn)
		if usd != nil {
			P("Net profit/day (USD):     %s%s", usdS(new(big.Rat).Quo(usd, new(big.Rat).SetFloat64(days))), warn)
			P("Monthly equivalent (USD): %s%s", usdS(new(big.Rat).Quo(new(big.Rat).Mul(usd, big.NewRat(30, 1)), new(big.Rat).SetFloat64(days))), warn)
		}
	}

	// ---- concentration ----
	head("PROFIT CONCENTRATION")
	if len(sorted) == 0 || total.Sign() == 0 {
		P("n/a (no positive net opportunity)")
	} else {
		desc := make([]*big.Int, len(sorted))
		for i := range sorted {
			desc[i] = sorted[len(sorted)-1-i]
		}
		for _, q := range []float64{1, 5, 10, 50} {
			k := int(float64(len(desc))*q/100 + 0.999999)
			if k < 1 {
				k = 1
			}
			P("Top %4.0f%% (%5d opps):     %5.1f%% of total net", q, k, 100*ratio(sum(desc[:k]), total))
		}
		acc, k := new(big.Int), 0
		half := new(big.Int).Quo(total, big.NewInt(2))
		for acc.Cmp(half) < 0 && k < len(desc) {
			acc.Add(acc, desc[k])
			k++
		}
		P("Opportunities making 50%% of profit: %d of %d", k, len(desc))
		P("Gini coefficient:         %.3f", gini(sorted))
	}

	// ---- capital ----
	head("CAPITAL  (flash-loan input L* of each opportunity)")
	var caps []*big.Int
	var rets []float64
	for _, o := range opps {
		caps = append(caps, o.C.L)
		rets = append(rets, ratio(o.Net, o.C.L))
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Cmp(caps[j]) < 0 })
	sort.Float64s(rets)
	if len(caps) > 0 {
		P("Median required capital:  %s", ethS(pctl(caps, 50)))
		P("P99 required capital:     %s", ethS(pctl(caps, 99)))
		P("Max required capital:     %s", ethS(caps[len(caps)-1]))
		P("Median net/capital:       %.4f%%", 100*rets[len(rets)/2])
		P("Own capital needed:       0 with a flash loan (lender liquidity: %s)", r.Cfg.FlashSource)
	}
	var life []float64
	for _, o := range opps {
		if o.E.Closed != nil {
			life = append(life, float64(o.E.Closed.Block-o.E.First.Block))
		}
	}
	sort.Float64s(life)
	if len(life) > 0 {
		P("Opportunity lifetime:     median %.0f blocks, max %.0f (closed ones; %d still open at end)", life[len(life)/2], life[len(life)-1], len(opps)-len(life))
	}

	// ---- visibility / competition evidence ----
	head("VISIBILITY & OBSERVED COMPETITION EVIDENCE")
	cls := map[string][2]*big.Int{}
	cnt := map[string]int{}
	closed := map[string]int{}
	for _, o := range opps {
		cnt[o.E.Class]++
		v := cls[o.E.Class]
		if v[0] == nil {
			v[0] = new(big.Int)
		}
		v[0].Add(v[0], o.Net)
		cls[o.E.Class] = v
		closed[o.E.ClosedBy]++
	}
	for _, k := range []string{"start_state", "public", "backrun_only"} {
		v := cls[k][0]
		P("%-14s %6d opps   %s", k, cnt[k], ethS(v))
	}
	P("  backrun_only = existed only right after a tx inside its block: capturable only by")
	P("  a searcher who saw that tx before inclusion (public mempool). NOT AVAILABLE: whether it was public.")
	P("")
	P("how opportunities ended (OBSERVED):")
	P("  closed by an on-chain arbitrage tx  %6d  %s", closed["arb_tx"], pct(closed["arb_tx"], len(opps)))
	P("  closed by another tx                %6d  %s", closed["other_tx"], pct(closed["other_tx"], len(opps)))
	P("  still open at end of range          %6d  %s", closed["open_at_end"], pct(closed["open_at_end"], len(opps)))
	P("Competition model:        NOT MODELED (captured share cannot be computed yet)")

	// ---- sensitivity ----
	head("SENSITIVITY: builder payment (same opportunities, net recomputed from pre-builder profit)")
	for _, bps := range []int64{0, 5000, 9000, 9900} {
		t := new(big.Int)
		n := 0
		for _, o := range opps {
			if o.C.PreBuilder == nil {
				continue
			}
			v := new(big.Int).Sub(o.C.PreBuilder, new(big.Int).Quo(new(big.Int).Mul(o.C.PreBuilder, big.NewInt(bps)), big.NewInt(10000)))
			if v.Sign() > 0 {
				t.Add(t, v)
				n++
			}
		}
		P("builder share %5.1f%%:   %6d opps   total net %s", float64(bps)/100, n, ethS(t))
	}

	// ---- tokens ----
	head("TOKEN BEHAVIOUR  (from pool->pool swap chains, OBSERVED)")
	ts := map[string]int{}
	for _, t := range r.Tokens {
		ts[t.Status]++
	}
	P("fee_on_transfer %d, anomalous %d, transfer_verified %d (tokens never seen in a chain: unverified)",
		ts["fee_on_transfer"], ts["anomalous"], ts["transfer_verified"])
	unv := new(big.Int)
	nu := 0
	for _, o := range opps {
		if o.C.UnverifiedTokens > 0 {
			unv.Add(unv, o.Net)
			nu++
		}
	}
	P("opportunities through >=1 UNVERIFIED token: %d of %d, %.1f%% of total net", nu, len(opps), 100*ratio(unv, total))
	P("  (unverified = may be honeypot / fee-on-transfer / blacklist; profit may not be real)")

	r.reportArbs(P, head)

	// ---- coverage ----
	head("COVERAGE  (what this scan does NOT see)")
	cv := r.Coverage
	P("Modeled V2 universe:      %d pools (Uniswap V2 + SushiSwap), %d tokens", cv.ModeledPools, cv.Tokens)
	P("Other V2-like factories:  %d active pools probed, NOT modeled", cv.OtherFactoryPools)
	for i, f := range cv.TopOtherFactories {
		if i == 5 {
			break
		}
		P("    %s  %s pools", f[0], f[1])
	}
	P("Non-pair Sync emitters:   %d", cv.NotPairEmitters)
	P("Untracked Sync logs:      %d from %d contracts", cv.UnknownSyncLogs, cv.UnknownEmitters)
	P("Unsupported DEX types:    Uniswap V3/V4, Curve, Balancer pools, CEX prices: NOT MODELED")
	P("Pools inactive in range:  NOT DISCOVERED (no events = not ingested)")

	// ---- assumptions ----
	head("ASSUMPTIONS & PROVENANCE")
	c := r.Cfg
	gh := []string{}
	for h := 2; h <= c.MaxHops; h++ {
		if g, ok := c.Gas.PerHops[h]; ok {
			gh = append(gh, fmt.Sprintf("%d-hop %d", h, g))
		} else {
			gh = append(gh, fmt.Sprintf("%d-hop NOT AVAILABLE", h))
		}
	}
	P("gas used (our tx):        %s  [%s] %s", strings.Join(gh, ", "), c.Gas.Source, c.Gas.Note)
	P("priority fee:             %s gwei  [%s] %s", new(big.Rat).SetFrac(c.Gas.PriorityWei, big.NewInt(1e9)).FloatString(3), c.Gas.PrioritySrc, c.Gas.PriorityNote)
	P("base fee:                 per block  [OBSERVED]")
	P("flash loan:               %s fee %s/%s, liquidity %s  [%s]", c.Flash.Name, c.Flash.FeeNum, c.Flash.FeeDen, ethS(c.Flash.MaxLiquidity), c.FlashSource)
	P("builder payment:          %d bps of profit after gas  [ASSUMED]", c.BuilderShareBps)
	P("min net profit:           %s wei  [ASSUMED]", c.MinProfitWei)
	if c.USDPool != "" {
		P("ETH/USD:                  V2 pool %s mid-price at top of block  [HISTORICALLY_RECONSTRUCTED]", c.USDPool)
	} else {
		P("ETH/USD:                  NOT AVAILABLE")
	}
	P("sizing:                   EXACT integer V2 math; L* within <= hops base units of integer optimum")
	P("competition:              NOT MODELED")

	r.report100k(P, head, opps, total, usd, days, small)
	P(bar)
}

func gini(asc []*big.Int) float64 {
	n := float64(len(asc))
	var s, w float64
	for i, v := range asc {
		f, _ := new(big.Float).SetInt(v).Float64()
		s += f
		w += float64(i+1) * f
	}
	if s == 0 {
		return 0
	}
	return (2*w)/(n*s) - (n+1)/n
}

func (r *Result) usdTotal(opps []Opp) *big.Rat {
	if len(opps) == 0 {
		return nil
	}
	t := new(big.Rat)
	for _, o := range opps {
		if o.C.NetUSD == nil {
			return nil
		}
		t.Add(t, o.C.NetUSD)
	}
	return t
}

func (r *Result) reportArbs(P func(string, ...any), head func(string)) {
	head("HISTORICAL ARBITRAGE VALIDATION  (on-chain closed V2 cycles in this range)")
	P("tx patterns: cycle_profit %d, cycle_loss %d, open_chain %d, unchained %d, pool_repeat %d, multi_sided %d, single %d",
		r.Patterns["cycle_profit"], r.Patterns["cycle_loss"], r.Patterns["open_chain"], r.Patterns["unchained"],
		r.Patterns["pool_repeat"], r.Patterns["multi_sided"], r.Patterns["single_swap"])
	P("  open_chain = normal multi-hop trade OR arbitrage with unmodeled (e.g. V3) legs: indistinguishable without traces")
	n := len(r.Arbs)
	if n == 0 {
		P("no closed profitable V2 cycle found on chain in this range")
		return
	}
	hop, rt, rec, inset := map[string]int{}, map[string]int{}, 0, 0
	why := map[string]int{}
	gasBy := map[int][]uint64{}
	var prios []*big.Int
	obsGross := new(big.Int)
	captures := 0
	for _, a := range r.Arbs {
		hop[a.ModelMatch]++
		rt[a.RouteMatch]++
		if a.InRouteSet {
			inset++
		}
		if a.Recognized {
			rec++
		} else {
			why[a.Reason]++
		}
		if a.GasUsed > 0 {
			gasBy[len(a.Arb.Legs)] = append(gasBy[len(a.Arb.Legs)], a.GasUsed)
		}
		if a.PriorityObs != nil {
			prios = append(prios, a.PriorityObs)
		}
		if a.Arb.Start == r.Cfg.Anchor {
			obsGross.Add(obsGross, a.Arb.Gross)
		}
		captures += len(a.Captures)
	}
	P("known arbitrage cycles:   %d", n)
	P("hop replay (observed in -> model out):  exact %d, below max %d, ABOVE MODEL (model error) %d, other %d",
		hop["exact"], hop["below_max"], hop["ABOVE_MODEL"], n-hop["exact"]-hop["below_max"]-hop["ABOVE_MODEL"])
	P("route replay (chained):                 exact %d, differs %d", rt["exact"], rt["differs"])
	P("in scanner route set:     %d", inset)
	P("recognized by scanner:    %d   (%s)", rec, pct(rec, n))
	P("not recognized:           %d", n-rec)
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		P("    %-40s %d", k, why[k])
	}
	P("observed gross (anchor-start cycles):  %s  [OBSERVED, before gas]", ethS(obsGross))
	// consistency: the scanner's optimum at the same state must be >= what
	// the on-chain tx achieved (up to the optimiser's <= hops units bound)
	ge, short, maxShort, worse := 0, 0, new(big.Int), 0
	for _, a := range r.Arbs {
		if !a.Recognized || a.ScanGross == nil || a.RouteMatch != "exact" {
			continue
		}
		d := new(big.Int).Sub(a.Arb.Gross, a.ScanGross)
		if d.Sign() <= 0 {
			ge++
			continue
		}
		short++
		if d.Cmp(maxShort) > 0 {
			maxShort.Set(d)
		}
		if d.Cmp(big.NewInt(int64(len(a.Arb.Legs)))) > 0 {
			worse++
		}
	}
	P("scanner optimum >= observed gross:      %d of %d; short by <= hops units %d (max %s wei); WORSE %d",
		ge, ge+short, short-worse, maxShort, worse)
	hs := make([]int, 0, len(gasBy))
	for h := range gasBy {
		hs = append(hs, h)
	}
	sort.Ints(hs)
	for _, h := range hs {
		g := gasBy[h]
		sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
		P("observed gasUsed %d-hop:  median %d, n=%d  [OBSERVED]", h, g[len(g)/2], len(g))
	}
	if len(prios) > 0 {
		sort.Slice(prios, func(i, j int) bool { return prios[i].Cmp(prios[j]) < 0 })
		P("observed priority fee:    median %s gwei, n=%d  [OBSERVED; coinbase transfers NOT AVAILABLE]",
			new(big.Rat).SetFrac(prios[len(prios)/2], big.NewInt(1e9)).FloatString(3), len(prios))
	}
	P("scanner episodes closed by these txs: %d", captures)
}

func (r *Result) report100k(P func(string, ...any), head func(string), opps []Opp, total *big.Int, usd *big.Rat, days float64, small bool) {
	head("$100K/MONTH ANALYSIS  (from measured distribution only)")
	if small {
		P("!!! SAMPLE TOO SMALL (%.2f days < %.0f). Numbers below are arithmetic on this sample,", days, MinReliableDays)
		P("!!! NOT a forecast. Scan more history before drawing conclusions.")
	}
	if usd == nil || days <= 0 {
		P("USD values NOT AVAILABLE -> comparison with $100K/month not possible in this run.")
		return
	}
	target := big.NewRat(TargetUSDPerMonth, 1)
	reqDay := new(big.Rat).Quo(target, big.NewRat(30, 1))
	month := new(big.Rat).Quo(new(big.Rat).Mul(usd, big.NewRat(30, 1)), new(big.Rat).SetFloat64(days))
	P("Required daily profit:          %s", usdS(reqDay))
	P("Observed net/day (100%% capture): %s", usdS(new(big.Rat).Quo(usd, new(big.Rat).SetFloat64(days))))
	P("Monthly equivalent:             %s", usdS(month))
	if month.Sign() <= 0 {
		P("Verdict input:                  no positive opportunity value in sample")
		return
	}
	need, _ := new(big.Rat).Quo(target, month).Float64()
	if need <= 1 {
		P("Required capture rate:          %.1f%% of all measured opportunities", need*100)
	} else {
		P("Shortfall:                      even 100%% capture is %.1fx BELOW $100K/month", need)
	}
	perOpp := new(big.Rat).Quo(usd, big.NewRat(int64(len(opps)), 1))
	P("Avg net per opportunity:        %s", usdS(perOpp))
	if perOpp.Sign() > 0 {
		n, _ := new(big.Rat).Quo(reqDay, perOpp).Float64()
		P("Executions/day needed at avg:   %.1f   (measured: %.1f opportunities/day)", n, float64(len(opps))/days)
	}
	P("")
	P("Scenarios (derived from this run's measured opportunity set):")
	sc := func(name string, keep func(o Opp) bool, scale *big.Rat) {
		t := new(big.Rat)
		for _, o := range opps {
			if keep(o) && o.C.NetUSD != nil {
				t.Add(t, o.C.NetUSD)
			}
		}
		t.Mul(t, scale)
		mo := new(big.Rat).Quo(new(big.Rat).Mul(t, big.NewRat(30, 1)), new(big.Rat).SetFloat64(days))
		f, _ := new(big.Rat).Quo(mo, target).Float64()
		P("  %-52s %14s/mo  (%5.1f%% of target)", name, usdS(mo), f*100)
	}
	all := func(Opp) bool { return true }
	sc("extreme: capture 100%, builder as configured", all, big.NewRat(1, 1))
	sc("strong: public + start_state only (no mempool needed)", func(o Opp) bool { return o.E.Class != "backrun_only" }, big.NewRat(1, 1))
	sc("median: verified tokens only", func(o Opp) bool { return o.C.UnverifiedTokens == 0 }, big.NewRat(1, 1))
	// conservative: drop the top 1% (outliers), verified tokens, 10% capture
	sorted := append([]Opp(nil), opps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Net.Cmp(sorted[j].Net) > 0 })
	k := len(sorted) / 100
	cut := map[int]bool{}
	for _, o := range sorted[:k] {
		cut[o.G.ID] = true
	}
	sc("conservative: no top-1%, verified tokens, 10% capture", func(o Opp) bool { return !cut[o.G.ID] && o.C.UnverifiedTokens == 0 }, big.NewRat(1, 10))
	P("  (capture rates are inputs, not measurements; competition is NOT MODELED)")
	_ = total
}
