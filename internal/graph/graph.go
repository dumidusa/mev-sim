// Package graph enumerates arbitrage cycles over V2 pools.
//
//	token = node, pool = edge (parallel edges allowed)
//
// Cycles must contain an anchor token (the token profit is measured in).
// Each DIRECTED cycle is produced exactly once:
//   - it starts at its smallest anchor (no rotation duplicates)
//   - no pool is used twice, no token is visited twice (except the return)
//
// The two directions of one cycle are different trades and are both kept;
// they share a CycleID and differ in RouteID.
package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

type Edge struct{ Pool, Token0, Token1 string }

type Route struct {
	RouteID string   // hash(start, pools in trade order)
	CycleID string   // same for both directions
	Forward bool     // true for the lexicographically smaller pool order
	Start   string   // anchor token, = Tokens[0] = Tokens[len-1]
	Pools   []string // trade order
	Tokens  []string // len(Pools)+1
}

func (r Route) Hops() int { return len(r.Pools) }

type Stats struct {
	TokensTotal, PoolsTotal       int
	TokensAfterPrune, PoolsInCore int
	AnchorsPresent                int
	Routes                        int
	RoutesByHops                  map[int]int
	Truncated                     bool // MaxRoutes reached: enumeration incomplete
}

type Set struct {
	Routes     []Route
	PoolRoutes map[string][]int // pool -> indexes into Routes
	Stats      Stats
}

type Config struct {
	Anchors   []string
	MaxHops   int // >= 2
	MaxRoutes int // 0 = unlimited
}

type adj struct {
	pool, to string
}

// Core returns the 2-core: tokens with at least 2 pools, applied repeatedly.
// A token with one pool cannot lie on any cycle.
func Core(edges []Edge) map[string][]adj {
	g := map[string][]adj{}
	for _, e := range edges {
		if e.Token0 == e.Token1 {
			continue
		}
		g[e.Token0] = append(g[e.Token0], adj{e.Pool, e.Token1})
		g[e.Token1] = append(g[e.Token1], adj{e.Pool, e.Token0})
	}
	for {
		removed := false
		for t, as := range g {
			if len(as) < 2 {
				delete(g, t)
				for _, a := range as {
					g[a.to] = drop(g[a.to], a.pool)
				}
				removed = true
			}
		}
		if !removed {
			break
		}
	}
	for t := range g {
		sort.Slice(g[t], func(i, j int) bool { return g[t][i].pool < g[t][j].pool })
	}
	return g
}

func drop(as []adj, pool string) []adj {
	out := as[:0]
	for _, a := range as {
		if a.pool != pool {
			out = append(out, a)
		}
	}
	return out
}

func bfs(g map[string][]adj, from string) map[string]int {
	d := map[string]int{from: 0}
	q := []string{from}
	for len(q) > 0 {
		u := q[0]
		q = q[1:]
		for _, a := range g[u] {
			if _, ok := d[a.to]; !ok {
				d[a.to] = d[u] + 1
				q = append(q, a.to)
			}
		}
	}
	return d
}

func hashParts(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:16])
}

func reversed(s []string) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[len(s)-1-i]
	}
	return out
}

func makeRoute(start string, pools, tokens []string) Route {
	rev := reversed(pools)
	fwd := strings.Join(pools, ",") <= strings.Join(rev, ",")
	canon := pools
	if !fwd {
		canon = rev
	}
	return Route{
		RouteID: hashParts(start, strings.Join(pools, ",")),
		CycleID: hashParts(start, strings.Join(canon, ",")),
		Forward: fwd, Start: start,
		Pools:  append([]string(nil), pools...),
		Tokens: append([]string(nil), tokens...),
	}
}

// Build enumerates all anchored simple cycles of 2..MaxHops pools.
func Build(edges []Edge, cfg Config) *Set {
	set := &Set{PoolRoutes: map[string][]int{}}
	st := &set.Stats
	st.RoutesByHops = map[int]int{}
	toks := map[string]bool{}
	for _, e := range edges {
		toks[e.Token0], toks[e.Token1] = true, true
	}
	st.TokensTotal, st.PoolsTotal = len(toks), len(edges)

	g := Core(edges)
	st.TokensAfterPrune = len(g)
	inCore := map[string]bool{}
	for _, as := range g {
		for _, a := range as {
			inCore[a.pool] = true
		}
	}
	st.PoolsInCore = len(inCore)

	anchors := append([]string(nil), cfg.Anchors...)
	sort.Strings(anchors)
	isAnchor := map[string]bool{}
	for _, a := range anchors {
		isAnchor[a] = true
	}

	for _, start := range anchors {
		if _, ok := g[start]; !ok {
			continue
		}
		st.AnchorsPresent++
		dist := bfs(g, start)
		pools := []string{}
		tokens := []string{start}
		usedPool := map[string]bool{}
		visited := map[string]bool{start: true}
		var dfs func(u string) bool
		dfs = func(u string) bool {
			d := len(pools)
			for _, a := range g[u] {
				if usedPool[a.pool] {
					continue
				}
				if a.to == start {
					if d+1 < 2 {
						continue
					}
					// canonical start: smallest anchor on the cycle
					ok := true
					for _, t := range tokens {
						if isAnchor[t] && t < start {
							ok = false
							break
						}
					}
					if !ok {
						continue
					}
					r := makeRoute(start, append(pools, a.pool), append(tokens, start))
					set.Routes = append(set.Routes, r)
					st.RoutesByHops[r.Hops()]++
					if cfg.MaxRoutes > 0 && len(set.Routes) >= cfg.MaxRoutes {
						st.Truncated = true
						return false
					}
					continue
				}
				if visited[a.to] || d+1 >= cfg.MaxHops {
					continue
				}
				// must still be able to get back to start in the hops left
				if dd, ok := dist[a.to]; !ok || dd > cfg.MaxHops-(d+1) {
					continue
				}
				usedPool[a.pool], visited[a.to] = true, true
				pools = append(pools, a.pool)
				tokens = append(tokens, a.to)
				cont := dfs(a.to)
				pools, tokens = pools[:len(pools)-1], tokens[:len(tokens)-1]
				delete(usedPool, a.pool)
				delete(visited, a.to)
				if !cont {
					return false
				}
			}
			return true
		}
		if !dfs(start) {
			break
		}
	}
	for i, r := range set.Routes {
		for _, p := range r.Pools {
			set.PoolRoutes[p] = append(set.PoolRoutes[p], i)
		}
	}
	st.Routes = len(set.Routes)
	return set
}

// RouteKey returns the RouteID a route with this start token and pool
// order has (used to match on-chain arbitrage txs to enumerated routes).
func RouteKey(start string, pools []string) string {
	return hashParts(start, strings.Join(pools, ","))
}
