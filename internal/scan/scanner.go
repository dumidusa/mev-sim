package scan

import (
	"database/sql"
	"fmt"
	"math/big"
	"sort"

	"github.com/dumidu/mev-sim/internal/amm"
	"github.com/dumidu/mev-sim/internal/graph"
	"github.com/dumidu/mev-sim/internal/histarb"
	"github.com/dumidu/mev-sim/internal/lifecycle"
	"github.com/dumidu/mev-sim/internal/route"
	"github.com/dumidu/mev-sim/internal/store"
	"github.com/dumidu/mev-sim/internal/uniswapv2"
)

// Episode = maximal run of scan points where one route stays gross-profitable.
type Episode struct {
	ID, Route              int
	First, Last            Point
	Closed                 *Point // nil = still open at end of range
	ClosedTx, ClosedBy     string // arb_tx | other_tx | open_at_end
	States                 int
	FirstCand, BestCand    int
	FirstValidCand         int // first candidate reaching ECONOMICALLY_VALID, -1 if none
	Public                 bool
	PublicFrom             uint64
	Class                  string // start_state | public | backrun_only
	GroupGross, GroupValid int
}

// Group = episodes born at the same point that share a pool: ONE opportunity.
type Group struct {
	ID          int
	Kind        string // gross | valid
	Point       Point
	Members     []int
	Best        int      // episode id
	Cand        int      // candidate id whose value is used
	Value       *big.Int // gross: first-state gross; valid: first-valid net
	Feasible    bool     // valid kind: best feasible member exists
	FeasibleVal *big.Int
}

type HopCheck struct {
	Pool          string
	ObsIn, ObsOut *big.Int
	ModelOut      *big.Int // getAmountOut(observed in) on reconstructed pre-state
	Result        string   // exact | below_max | ABOVE_MODEL
}

type ArbResult struct {
	Arb         histarb.Arb
	Pre         Point // state before the tx = after tx index-1
	Hops        []HopCheck
	ModelMatch  string   // exact | below_max | ABOVE_MODEL (model error)
	RouteSimOut *big.Int // chained simulation of the observed route with observed input
	RouteMatch  string   // exact | differs (chained model output vs observed final output)
	RouteID     string
	InRouteSet  bool
	Recognized  bool
	Reason      string
	ScanStage   lifecycle.Stage
	ScanL       *big.Int
	ScanGross   *big.Int
	GasUsed     uint64
	EffGasPrice *big.Int
	BaseFee     *big.Int
	PriorityObs *big.Int
	GasCostObs  *big.Int
	Captures    []int // episodes this tx closed (it ran on their pools)
}

type Result struct {
	Cfg        Config
	Set        *graph.Set
	Cands      []*Candidate
	Episodes   []*Episode
	Groups     []*Group
	Arbs       []*ArbResult
	Patterns   map[string]int
	Tokens     []*histarb.TokenEvidence
	Metrics    map[string]int64
	Blocks     map[uint64]BlockInfo
	FirstBlock uint64 // first block with a stored header in range
	Coverage   Coverage
}

type Coverage struct {
	ModeledPools, PoolsInCore, Tokens, TokensInCore     int
	OtherFactoryPools, NotPairEmitters, UnknownEmitters int
	UnknownSyncLogs                                     int64
	TopOtherFactories                                   [][2]string
}

type scanner struct {
	st        *store.Store
	cfg       Config
	ev        *evaluator
	set       *graph.Set
	routeByID map[string]int
	state     route.State
	pools     map[string]store.Pool
	lastFP    []string
	open      []int
	res       *Result
	arbsByTx  map[[2]uint64][]histarb.Arb
	initial   Point
}

func (s *scanner) metric(k string, n int64) { s.res.Metrics[k] += n }

func setPool(state route.State, p store.Pool, r0, r1 *big.Int) error {
	v, err := amm.NewV2Pool(amm.Address(p.Address), amm.Address(p.Token0), amm.Address(p.Token1), r0, r1, p.FeeBps, p.Factory)
	if err != nil {
		return err
	}
	state[amm.Address(p.Address)] = v
	return nil
}

func (s *scanner) active(r graph.Route) bool {
	for _, p := range r.Pools {
		if _, ok := s.state[amm.Address(p)]; !ok {
			return false
		}
	}
	return true
}

// Run executes a deterministic scan over [cfg.Start, cfg.End].
func Run(st *store.Store, cfg Config) (*Result, error) {
	if cfg.MinProfitWei == nil {
		cfg.MinProfitWei = big.NewInt(1)
	}
	if cfg.MaxHops < 2 {
		return nil, fmt.Errorf("max-hops must be >= 2")
	}
	s := &scanner{st: st, cfg: cfg, res: &Result{Metrics: map[string]int64{}, Patterns: map[string]int{}}}
	var err error
	if s.pools, err = st.Pools(); err != nil {
		return nil, err
	}
	dbStartS, ok, err := st.Meta("start_block")
	if err != nil || !ok {
		return nil, fmt.Errorf("db has no start_block (run ingest first)")
	}
	var dbStart uint64
	fmt.Sscan(dbStartS, &dbStart)
	done, _, _ := st.Progress("blocks")
	if cfg.Start < dbStart || cfg.End > done || cfg.End < cfg.Start {
		return nil, fmt.Errorf("scan range %d..%d outside ingested range %d..%d", cfg.Start, cfg.End, dbStart, done)
	}

	// --- observed history: swaps, token behaviour, on-chain arbitrage ---
	txs, err := histarb.Load(st, cfg.Start, cfg.End)
	if err != nil {
		return nil, err
	}
	ev := histarb.Tokens(txs)
	s.res.Tokens = histarb.SortedTokens(ev)
	tokStatus := map[string]string{}
	for t, e := range ev {
		tokStatus[t] = e.Status
	}
	s.arbsByTx = map[[2]uint64][]histarb.Arb{}
	for _, tx := range txs {
		pat, arbs := histarb.Classify(tx)
		s.res.Patterns[pat]++
		if len(arbs) > 0 {
			s.arbsByTx[[2]uint64{tx.Block, tx.TxIndex}] = arbs
		}
	}

	// --- blocks (base fee OBSERVED) ---
	s.res.Blocks = map[uint64]BlockInfo{}
	err = st.Blocks(cfg.Start, cfg.End, func(b store.Block) error {
		s.res.Blocks[b.Number] = BlockInfo{Number: b.Number, Timestamp: b.Timestamp, GasLimit: b.GasLimit, BaseFee: b.BaseFee}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(s.res.Blocks) != int(cfg.End-cfg.Start+1) {
		return nil, fmt.Errorf("blocks table has %d of %d headers in range (re-run ingest)", len(s.res.Blocks), cfg.End-cfg.Start+1)
	}

	// --- gas model ---
	if cfg.Gas.PerHops == nil || cfg.Gas.PriorityWei == nil {
		est, err := EstimateGas(st, s.arbsByTx)
		if err != nil {
			return nil, err
		}
		if cfg.Gas.PerHops == nil {
			cfg.Gas.PerHops, cfg.Gas.Source, cfg.Gas.Note = est.PerHops, est.Source, est.Note
		}
		if cfg.Gas.PriorityWei == nil {
			cfg.Gas.PriorityWei, cfg.Gas.PrioritySrc, cfg.Gas.PriorityNote = est.PriorityWei, est.PrioritySrc, est.PriorityNote
		}
	}
	s.cfg = cfg
	s.res.Cfg = cfg
	s.ev = &evaluator{cfg: cfg, tokens: tokStatus}

	// --- route graph ---
	var edges []graph.Edge
	addrs := make([]string, 0, len(s.pools))
	for a := range s.pools {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		p := s.pools[a]
		edges = append(edges, graph.Edge{Pool: a, Token0: p.Token0, Token1: p.Token1})
	}
	s.set = graph.Build(edges, graph.Config{Anchors: []string{cfg.Anchor}, MaxHops: cfg.MaxHops, MaxRoutes: cfg.MaxRoutes})
	s.res.Set = s.set
	s.routeByID = map[string]int{}
	for i, r := range s.set.Routes {
		s.routeByID[r.RouteID] = i
	}
	s.lastFP = make([]string, len(s.set.Routes))
	s.open = make([]int, len(s.set.Routes))
	for i := range s.open {
		s.open[i] = -1
	}
	if err := s.coverage(); err != nil {
		return nil, err
	}

	// --- initial state = seeds (state at end of dbStart-1) ---
	seeds, err := st.Seeds()
	if err != nil {
		return nil, err
	}
	s.state = route.State{}
	for a, sd := range seeds {
		if p, ok := s.pools[a]; ok {
			if err := setPool(s.state, p, sd.Reserve0, sd.Reserve1); err != nil {
				return nil, err
			}
		}
	}
	// fast-forward (no scanning) from the db start to the scan start
	if cfg.Start > dbStart {
		err = st.Events(dbStart, cfg.Start-1, func(e store.Event) error { return s.applyEvent(e, nil) })
		if err != nil {
			return nil, err
		}
	}

	// --- scan ---
	s.initial = Point{cfg.Start, -1}
	s.topOfBlock(cfg.Start, cfg.Start)
	all := make([]int, len(s.set.Routes))
	for i := range all {
		all[i] = i
	}
	if err := s.scanPoint(s.initial, "", all, false); err != nil {
		return nil, err
	}
	s.metric("scan_points", 1)

	var cur *[2]uint64
	var curHash string
	changed := map[string]bool{}
	curBlock := cfg.Start
	flush := func() error {
		if cur == nil {
			return nil
		}
		pt := Point{cur[0], int64(cur[1])}
		var aff []int
		seen := map[int]bool{}
		ps := make([]string, 0, len(changed))
		for p := range changed {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		for _, p := range ps {
			for _, i := range s.set.PoolRoutes[p] {
				if !seen[i] {
					seen[i] = true
					aff = append(aff, i)
				}
			}
		}
		sort.Ints(aff)
		_, isArb := s.arbsByTx[*cur]
		s.metric("scan_points", 1)
		changed = map[string]bool{}
		return s.scanPoint(pt, curHash, aff, isArb)
	}
	err = st.Events(cfg.Start, cfg.End, func(e store.Event) error {
		k := e.Key()
		key := [2]uint64{k.Block, k.TxIndex}
		if cur == nil || *cur != key {
			if err := flush(); err != nil {
				return err
			}
			if k.Block > curBlock {
				s.topOfBlock(k.Block, curBlock+1)
				curBlock = k.Block
			}
			cur, curHash = &key, k.TxHash
			if arbs, ok := s.arbsByTx[key]; ok {
				if err := s.checkArbs(arbs); err != nil {
					return err
				}
			}
		}
		return s.applyEvent(e, changed)
	})
	if err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	s.finish(curBlock)
	s.groups()
	return s.res, nil
}

func (s *scanner) applyEvent(e store.Event, changed map[string]bool) error {
	if e.Sync == nil {
		return nil
	}
	p, ok := s.pools[e.Sync.Pool]
	if !ok {
		return nil
	}
	if _, seeded := s.state[amm.Address(p.Address)]; !seeded {
		return fmt.Errorf("sync on unseeded pool %s at %d", p.Address, e.Sync.Block)
	}
	if changed != nil {
		changed[p.Address] = true
	}
	return setPool(s.state, p, e.Sync.Reserve0, e.Sync.Reserve1)
}

// topOfBlock: called before the first tx of block b. Every open episode
// existed at the top of block `from` (the first block after the one it was
// last confirmed in) -> it was publicly visible, no mempool access needed.
func (s *scanner) topOfBlock(b, from uint64) {
	for _, ei := range s.open {
		if ei >= 0 && !s.res.Episodes[ei].Public {
			s.res.Episodes[ei].Public, s.res.Episodes[ei].PublicFrom = true, from
		}
	}
	bi := s.res.Blocks[b]
	bi.ETHUSD = s.ethUSD()
	s.res.Blocks[b] = bi
}

// ethUSD: mid-price of the configured V2 USD/WETH pool in the current state.
func (s *scanner) ethUSD() *big.Rat {
	if s.cfg.USDPool == "" {
		return nil
	}
	v, ok := s.state[amm.Address(s.cfg.USDPool)].(*amm.V2Pool)
	if !ok {
		return nil
	}
	t0, t1 := v.Tokens()
	r0, r1 := v.Reserves()
	var usd, weth *big.Int
	switch {
	case string(t0) == s.cfg.USDToken && string(t1) == s.cfg.Anchor:
		usd, weth = r0, r1
	case string(t1) == s.cfg.USDToken && string(t0) == s.cfg.Anchor:
		usd, weth = r1, r0
	default:
		return nil
	}
	if weth.Sign() == 0 || usd.Sign() == 0 {
		return nil
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(18-s.cfg.USDDecimals)), nil)
	return new(big.Rat).SetFrac(new(big.Int).Mul(usd, scale), weth)
}

func (s *scanner) scanPoint(pt Point, trigger string, routes []int, triggerIsArb bool) error {
	blk := s.res.Blocks[pt.Block]
	for _, ri := range routes {
		r := s.set.Routes[ri]
		if !s.active(r) {
			s.metric("route_skipped_inactive", 1)
			continue
		}
		fp := fingerprint(s.state, r)
		if fp == s.lastFP[ri] {
			s.metric("route_skipped_same_state", 1)
			continue
		}
		s.lastFP[ri] = fp
		s.metric("route_evaluations", 1)
		c, err := s.ev.evaluate(s.state, r, pt, blk)
		if err != nil {
			return fmt.Errorf("route %s at %s: %w", r.RouteID, pt, err)
		}
		profitable := false
		if c == nil {
			s.metric("no_discrepancy", 1)
		} else {
			c.ID, c.Route, c.TriggerTx, c.Fingerprint = len(s.res.Cands), ri, trigger, fp
			s.res.Cands = append(s.res.Cands, c)
			profitable = c.reached(lifecycle.GrossProfitable)
		}
		ei := s.open[ri]
		switch {
		case profitable && ei < 0:
			e := &Episode{ID: len(s.res.Episodes), Route: ri, First: pt, Last: pt, FirstCand: c.ID, BestCand: c.ID,
				FirstValidCand: -1, GroupGross: -1, GroupValid: -1}
			s.res.Episodes = append(s.res.Episodes, e)
			s.open[ri] = e.ID
			s.addState(e, c)
		case profitable:
			s.addState(s.res.Episodes[ei], c)
		case ei >= 0:
			e := s.res.Episodes[ei]
			p := pt
			e.Closed, e.ClosedTx = &p, trigger
			e.ClosedBy = "other_tx"
			if triggerIsArb {
				e.ClosedBy = "arb_tx"
			}
			s.open[ri] = -1
		}
	}
	return nil
}

func (s *scanner) addState(e *Episode, c *Candidate) {
	c.Episode = e.ID
	e.States++
	e.Last = c.Point
	if b := s.res.Cands[e.BestCand]; netOrGross(c).Cmp(netOrGross(b)) > 0 {
		e.BestCand = c.ID
	}
	if e.FirstValidCand < 0 && c.reached(lifecycle.EconomicallyValid) {
		e.FirstValidCand = c.ID
	}
}

// netOrGross ranks candidates: net when it reached GAS stage, else pre-gas.
func netOrGross(c *Candidate) *big.Int {
	if c.Net != nil {
		return c.Net
	}
	if c.PreBuilder != nil {
		return c.PreBuilder
	}
	if c.FlashFee != nil {
		return new(big.Int).Sub(c.Gross, c.FlashFee)
	}
	return c.Gross
}

func (s *scanner) finish(lastBlock uint64) {
	for ri, ei := range s.open {
		if ei < 0 {
			continue
		}
		e := s.res.Episodes[ei]
		e.ClosedBy = "open_at_end"
		if lastBlock < s.cfg.End && !e.Public { // survived into later (event-free) blocks
			e.Public, e.PublicFrom = true, lastBlock+1
		}
		s.open[ri] = -1
	}
	for _, e := range s.res.Episodes {
		switch {
		case e.First == s.initial:
			e.Class = "start_state"
		case e.Public:
			e.Class = "public"
		default:
			e.Class = "backrun_only"
		}
	}
}

// ---- on-chain arbitrage replay ------------------------------------------

func (s *scanner) checkArbs(arbs []histarb.Arb) error {
	temp := route.State{} // pools mutated by earlier legs of this same tx
	get := func(p string) (*amm.V2Pool, bool) {
		if v, ok := temp[amm.Address(p)]; ok {
			return v.(*amm.V2Pool), true
		}
		v, ok := s.state[amm.Address(p)].(*amm.V2Pool)
		return v, ok
	}
	for _, a := range arbs {
		r := &ArbResult{Arb: a, Pre: Point{a.Block, int64(a.TxIndex) - 1}, ModelMatch: "exact"}
		// route-level replay on the state this sub-cycle saw
		view := route.State{}
		var rr route.Route
		for _, l := range a.Legs {
			if v, ok := get(l.Pool); ok {
				view[amm.Address(l.Pool)] = v
			}
			rr.Hops = append(rr.Hops, route.H(amm.Address(l.Pool), amm.Address(l.TokenIn), amm.Address(l.TokenOut)))
		}
		r.RouteMatch = "not_simulated"
		if sim, err := route.Simulate(view, rr, a.AmountIn); err == nil {
			r.RouteSimOut = sim.AmountOut
			r.RouteMatch = "exact"
			if sim.AmountOut.Cmp(a.AmountOut) != 0 {
				r.RouteMatch = "differs" // e.g. fee-on-transfer between hops, or a bot asking below max
			}
		}
		// hop-level: each observed input through the reconstructed pool
		for _, l := range a.Legs {
			hc := HopCheck{Pool: l.Pool, ObsIn: l.AmountIn, ObsOut: l.AmountOut}
			v, ok := get(l.Pool)
			if !ok {
				hc.Result = "pool_not_in_state"
				r.ModelMatch = "pool_not_in_state"
				r.Hops = append(r.Hops, hc)
				continue
			}
			c := v.Clone().(*amm.V2Pool)
			out, err := c.QuoteExactIn(amm.Address(l.TokenIn), l.AmountIn)
			if err != nil {
				hc.Result = "model_reverts"
				r.ModelMatch = "ABOVE_MODEL"
			} else {
				hc.ModelOut = out
				switch l.AmountOut.Cmp(out) {
				case 0:
					hc.Result = "exact"
				case -1:
					hc.Result = "below_max"
					if r.ModelMatch == "exact" {
						r.ModelMatch = "below_max"
					}
				default:
					hc.Result = "ABOVE_MODEL"
					r.ModelMatch = "ABOVE_MODEL"
				}
			}
			r.Hops = append(r.Hops, hc)
			// advance the temp pool exactly as the chain did: +in, -out
			r0, r1 := v.Reserves()
			t0, _ := v.Tokens()
			if string(t0) == l.TokenIn {
				r0.Add(r0, l.AmountIn)
				r1.Sub(r1, l.AmountOut)
			} else {
				r1.Add(r1, l.AmountIn)
				r0.Sub(r0, l.AmountOut)
			}
			if r0.Sign() >= 0 && r1.Sign() >= 0 {
				_ = setPool(temp, s.pools[l.Pool], r0, r1)
			}
		}
		s.recognize(r, view)
		s.observedGas(r)
		s.res.Arbs = append(s.res.Arbs, r)
	}
	return nil
}

// recognize: would the scanner have flagged this route at this state?
func (s *scanner) recognize(r *ArbResult, view route.State) {
	a := r.Arb
	toks := a.Tokens()
	pools := a.Pools()
	j := -1
	for i := 0; i < len(pools); i++ {
		if toks[i] == s.cfg.Anchor {
			j = i
			break
		}
	}
	switch {
	case j < 0:
		r.Reason = "no_anchor_token_in_cycle"
		return
	case len(pools) > s.cfg.MaxHops:
		r.Reason = fmt.Sprintf("hops_%d_exceed_max_%d", len(pools), s.cfg.MaxHops)
		return
	}
	rot := append(append([]string{}, pools[j:]...), pools[:j]...)
	r.RouteID = graph.RouteKey(s.cfg.Anchor, rot)
	ri, ok := s.routeByID[r.RouteID]
	if !ok {
		r.Reason = "route_not_enumerated"
		return
	}
	r.InRouteSet = true
	gr := s.set.Routes[ri]
	st := route.State{}
	for _, p := range gr.Pools {
		v, ok := view[amm.Address(p)]
		if !ok {
			r.Reason = "pool_not_in_state"
			return
		}
		st[amm.Address(p)] = v
	}
	c, err := s.ev.evaluate(st, gr, r.Pre, s.res.Blocks[a.Block])
	if err != nil {
		r.Reason = "evaluation_error: " + err.Error()
		return
	}
	if c == nil {
		r.Reason = "screen_no_discrepancy"
		r.ScanStage = "NONE"
		return
	}
	r.ScanStage, r.ScanL, r.ScanGross = c.Stage, c.L, c.Gross
	if c.reached(lifecycle.GrossProfitable) {
		r.Recognized = true
		r.Reason = "recognized"
	} else {
		r.Reason = string(c.Reason)
	}
}

func (s *scanner) observedGas(r *ArbResult) {
	var gu uint64
	var egp string
	err := s.st.DB.QueryRow(`SELECT gas_used, effective_gas_price FROM transactions WHERE block_number=? AND tx_index=?`,
		r.Arb.Block, r.Arb.TxIndex).Scan(&gu, &egp)
	if err == sql.ErrNoRows || err != nil {
		return
	}
	r.GasUsed = gu
	r.EffGasPrice, _ = new(big.Int).SetString(egp, 10)
	r.BaseFee = s.res.Blocks[r.Arb.Block].BaseFee
	if r.EffGasPrice != nil {
		r.GasCostObs = new(big.Int).Mul(r.EffGasPrice, new(big.Int).SetUint64(gu))
		if r.BaseFee != nil {
			r.PriorityObs = new(big.Int).Sub(r.EffGasPrice, r.BaseFee)
		}
	}
}

// ---- grouping -------------------------------------------------------------

func (s *scanner) groups() {
	// tag episodes closed by an observed arbitrage tx
	for _, a := range s.res.Arbs {
		for _, e := range s.res.Episodes {
			if e.ClosedBy == "arb_tx" && e.ClosedTx == a.Arb.TxHash {
				a.Captures = append(a.Captures, e.ID)
			}
		}
	}
	build := func(kind string, at func(e *Episode) (Point, int, bool)) {
		byPoint := map[Point][]int{}
		var pts []Point
		for _, e := range s.res.Episodes {
			p, _, ok := at(e)
			if !ok {
				continue
			}
			if _, seen := byPoint[p]; !seen {
				pts = append(pts, p)
			}
			byPoint[p] = append(byPoint[p], e.ID)
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].Less(pts[j]) })
		for _, p := range pts {
			ids := byPoint[p]
			parent := map[int]int{}
			var find func(int) int
			find = func(x int) int {
				for parent[x] != x {
					parent[x] = parent[parent[x]]
					x = parent[x]
				}
				return x
			}
			owner := map[string]int{}
			for _, id := range ids {
				parent[id] = id
			}
			for _, id := range ids {
				for _, pool := range s.set.Routes[s.res.Episodes[id].Route].Pools {
					if o, ok := owner[pool]; ok {
						parent[find(id)] = find(o)
					} else {
						owner[pool] = id
					}
				}
			}
			comps := map[int][]int{}
			var roots []int
			for _, id := range ids {
				rt := find(id)
				if _, ok := comps[rt]; !ok {
					roots = append(roots, rt)
				}
				comps[rt] = append(comps[rt], id)
			}
			sort.Ints(roots)
			for _, rt := range roots {
				g := &Group{ID: len(s.res.Groups), Kind: kind, Point: p, Members: comps[rt], Best: -1}
				for _, id := range g.Members {
					e := s.res.Episodes[id]
					_, ci, _ := at(e)
					c := s.res.Cands[ci]
					v := c.Gross
					if kind == "valid" {
						v = c.Net
						e.GroupValid = g.ID
						if c.reached(lifecycle.ExecutionFeasible) && (g.FeasibleVal == nil || v.Cmp(g.FeasibleVal) > 0) {
							g.Feasible, g.FeasibleVal = true, v
						}
					} else {
						e.GroupGross = g.ID
					}
					if g.Value == nil || v.Cmp(g.Value) > 0 {
						g.Value, g.Best, g.Cand = v, id, ci
					}
				}
				s.res.Groups = append(s.res.Groups, g)
			}
		}
	}
	build("gross", func(e *Episode) (Point, int, bool) { return e.First, e.FirstCand, true })
	build("valid", func(e *Episode) (Point, int, bool) {
		if e.FirstValidCand < 0 {
			return Point{}, 0, false
		}
		return s.res.Cands[e.FirstValidCand].Point, e.FirstValidCand, true
	})
}

func (s *scanner) coverage() error {
	cv := &s.res.Coverage
	cv.ModeledPools, cv.PoolsInCore = s.set.Stats.PoolsTotal, s.set.Stats.PoolsInCore
	cv.Tokens, cv.TokensInCore = s.set.Stats.TokensTotal, s.set.Stats.TokensAfterPrune
	probes, err := s.st.Probes()
	if err != nil {
		return err
	}
	other := map[string]int{}
	for _, p := range probes {
		switch p.Status {
		case store.ProbeOtherFactory:
			cv.OtherFactoryPools++
			other[p.Factory]++
		case store.ProbeNotPair:
			cv.NotPairEmitters++
		}
	}
	for f, n := range other {
		cv.TopOtherFactories = append(cv.TopOtherFactories, [2]string{f, fmt.Sprint(n)})
	}
	sort.Slice(cv.TopOtherFactories, func(i, j int) bool {
		a, b := cv.TopOtherFactories[i], cv.TopOtherFactories[j]
		var x, y int
		fmt.Sscan(a[1], &x)
		fmt.Sscan(b[1], &y)
		return x > y || (x == y && a[0] < b[0])
	})
	return s.st.DB.QueryRow(`SELECT COUNT(DISTINCT address), COALESCE(SUM(count),0) FROM unknown_emitters WHERE topic=?`,
		uniswapv2.TopicSync).Scan(&cv.UnknownEmitters, &cv.UnknownSyncLogs)
}
