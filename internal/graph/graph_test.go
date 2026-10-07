package graph

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

const W, U, D, T = "weth", "usdc", "dai", "usdt"

func E(p, a, b string) Edge { return Edge{p, a, b} }

func routeStrings(s *Set) []string {
	var out []string
	for _, r := range s.Routes {
		out = append(out, strings.Join(r.Pools, ">"))
	}
	sort.Strings(out)
	return out
}

func TestTwoHopBothDirections(t *testing.T) {
	s := Build([]Edge{E("A", W, U), E("B", W, U)}, Config{Anchors: []string{W}, MaxHops: 2})
	got := routeStrings(s)
	if fmt.Sprint(got) != "[A>B B>A]" {
		t.Fatalf("got %v", got)
	}
	if s.Routes[0].CycleID != s.Routes[1].CycleID || s.Routes[0].RouteID == s.Routes[1].RouteID {
		t.Fatal("directions must share CycleID and differ in RouteID")
	}
	if s.Routes[0].Forward == s.Routes[1].Forward {
		t.Fatal("exactly one direction is Forward")
	}
}

func TestSinglePoolIsNoCycle(t *testing.T) {
	s := Build([]Edge{E("A", W, U)}, Config{Anchors: []string{W}, MaxHops: 4})
	if len(s.Routes) != 0 {
		t.Fatal("a single pool cannot be a cycle (pool reuse)")
	}
}

func TestTriangleAndMaxHops(t *testing.T) {
	edges := []Edge{E("P1", W, U), E("P2", U, D), E("P3", D, W)}
	if n := len(Build(edges, Config{Anchors: []string{W}, MaxHops: 2}).Routes); n != 0 {
		t.Fatalf("max_hops=2 found %d 3-hop routes", n)
	}
	s := Build(edges, Config{Anchors: []string{W}, MaxHops: 3})
	if fmt.Sprint(routeStrings(s)) != "[P1>P2>P3 P3>P2>P1]" {
		t.Fatalf("got %v", routeStrings(s))
	}
	r := s.Routes[0]
	if r.Tokens[0] != W || r.Tokens[3] != W || len(r.Tokens) != 4 {
		t.Fatalf("tokens %v", r.Tokens)
	}
}

// Two anchors on one cycle: found once per direction, starting at the smaller anchor.
func TestNoRotationDuplicatesWithTwoAnchors(t *testing.T) {
	edges := []Edge{E("P1", W, U), E("P2", U, D), E("P3", D, W)}
	s := Build(edges, Config{Anchors: []string{W, U}, MaxHops: 3})
	if len(s.Routes) != 2 {
		t.Fatalf("got %d routes, want 2: %v", len(s.Routes), routeStrings(s))
	}
	for _, r := range s.Routes {
		if r.Start != U { // "usdc" < "weth"
			t.Fatalf("start %s, want smallest anchor", r.Start)
		}
	}
}

// Leaf tokens are pruned and never appear in routes.
func TestCorePrunesLeaves(t *testing.T) {
	edges := []Edge{E("A", W, U), E("B", W, U), E("L1", U, "leaf"), E("L2", "leaf", "leaf2")}
	s := Build(edges, Config{Anchors: []string{W}, MaxHops: 4})
	if s.Stats.TokensAfterPrune != 2 || s.Stats.PoolsInCore != 2 {
		t.Fatalf("prune: %+v", s.Stats)
	}
	for _, r := range s.Routes {
		for _, p := range r.Pools {
			if strings.HasPrefix(p, "L") {
				t.Fatal("pruned pool in route")
			}
		}
	}
}

// Brute force: every permutation of distinct pools forming a closed,
// token-simple path from the anchor. Must equal Build exactly.
func bruteForce(edges []Edge, anchor string, maxHops int) []string {
	var out []string
	var rec func(path []int, tok string, seen map[string]bool)
	rec = func(path []int, tok string, seen map[string]bool) {
		if len(path) >= maxHops {
			return
		}
		for i, e := range edges {
			used := false
			for _, j := range path {
				used = used || j == i
			}
			if used {
				continue
			}
			var next string
			switch tok {
			case e.Token0:
				next = e.Token1
			case e.Token1:
				next = e.Token0
			default:
				continue
			}
			p := append(append([]int(nil), path...), i)
			if next == anchor {
				if len(p) >= 2 {
					var names []string
					for _, j := range p {
						names = append(names, edges[j].Pool)
					}
					out = append(out, strings.Join(names, ">"))
				}
				continue
			}
			if seen[next] {
				continue
			}
			s2 := map[string]bool{next: true}
			for k := range seen {
				s2[k] = true
			}
			rec(p, next, s2)
		}
	}
	rec(nil, anchor, map[string]bool{anchor: true})
	sort.Strings(out)
	return out
}

func TestMatchesBruteForceOnRandomGraphs(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	toks := []string{W, U, D, T, "x", "y"}
	for trial := 0; trial < 60; trial++ {
		var edges []Edge
		n := 3 + rng.Intn(8)
		for i := 0; i < n; i++ {
			a, b := toks[rng.Intn(len(toks))], toks[rng.Intn(len(toks))]
			if a == b {
				continue
			}
			edges = append(edges, E(fmt.Sprintf("p%02d", i), a, b))
		}
		for _, mh := range []int{2, 3, 4} {
			want := bruteForce(edges, W, mh)
			got := routeStrings(Build(edges, Config{Anchors: []string{W}, MaxHops: mh}))
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("trial %d maxHops %d\nedges %v\nwant %v\ngot  %v", trial, mh, edges, want, got)
			}
		}
	}
}

func TestPoolIndexAndUniqueIDs(t *testing.T) {
	edges := []Edge{E("A", W, U), E("B", W, U), E("C", U, D), E("D", D, W), E("E", W, D)}
	s := Build(edges, Config{Anchors: []string{W}, MaxHops: 3})
	ids := map[string]bool{}
	for i, r := range s.Routes {
		if ids[r.RouteID] {
			t.Fatal("duplicate RouteID")
		}
		ids[r.RouteID] = true
		for _, p := range r.Pools {
			found := false
			for _, j := range s.PoolRoutes[p] {
				found = found || j == i
			}
			if !found {
				t.Fatalf("route %d missing from index of %s", i, p)
			}
		}
	}
}

func TestDeterministicAndTruncationReported(t *testing.T) {
	edges := []Edge{E("A", W, U), E("B", W, U), E("C", U, D), E("D", D, W), E("E", W, D)}
	a := Build(edges, Config{Anchors: []string{W}, MaxHops: 3})
	b := Build(append([]Edge{edges[4], edges[2]}, edges[0], edges[3], edges[1]), Config{Anchors: []string{W}, MaxHops: 3})
	for i := range a.Routes {
		if a.Routes[i].RouteID != b.Routes[i].RouteID {
			t.Fatal("route order depends on input order")
		}
	}
	c := Build(edges, Config{Anchors: []string{W}, MaxHops: 3, MaxRoutes: 3})
	if !c.Stats.Truncated || len(c.Routes) != 3 {
		t.Fatalf("truncation not reported: %+v", c.Stats)
	}
}
