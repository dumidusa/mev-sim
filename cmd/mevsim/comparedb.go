package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/dumidu/mev-sim/internal/dbcompare"
	"github.com/dumidu/mev-sim/internal/store"
)

func cmdCompareDB(args []string) int {
	fs := flag.NewFlagSet("compare-db", flag.ExitOnError)
	a := fs.String("a", "", "first db (e.g. RPC-ingested)")
	b := fs.String("b", "", "second db (e.g. BigQuery-ingested)")
	from := fs.Uint64("start-block", 0, "range start (block-keyed tables)")
	to := fs.Uint64("end-block", 0, "range end")
	ign := fs.String("ignore", "transactions.selector", "comma list table.column known to differ by source")
	fs.Parse(args)
	if *a == "" || *b == "" || *to == 0 {
		log.Fatal("need --a --b --start-block --end-block")
	}
	sa, err := store.Open(*a)
	if err != nil {
		log.Fatal(err)
	}
	defer sa.Close()
	sb, err := store.Open(*b)
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Close()
	diffs, err := dbcompare.Compare(sa.DB, sb.DB, *from, *to)
	if err != nil {
		log.Fatal(err)
	}
	ignore := map[string]bool{}
	for _, x := range strings.Split(*ign, ",") {
		if x != "" {
			ignore[x] = true
		}
	}
	ok := true
	fmt.Printf("%-18s %8s %8s %7s %7s %8s  %s\n", "table", "rows A", "rows B", "only A", "only B", "differ", "result")
	for _, d := range diffs {
		res := "IDENTICAL"
		if !d.Clean(ignore) {
			res, ok = "DIFFERENT", false
		} else if d.RowsDiffer > 0 {
			res = "same (ignored cols differ)"
		}
		fmt.Printf("%-18s %8d %8d %7d %7d %8d  %s\n", d.Table, d.RowsA, d.RowsB, d.OnlyA, d.OnlyB, d.RowsDiffer, res)
		for c, n := range d.Cols {
			tag := ""
			if ignore[d.Table+"."+c] {
				tag = " (ignored)"
			}
			fmt.Printf("    column %-22s differs in %d rows%s\n", c, n, tag)
		}
		if res == "DIFFERENT" {
			for _, e := range d.Examples {
				fmt.Println("    e.g. " + e)
			}
		}
	}
	if ok {
		fmt.Println("RESULT: EQUIVALENT")
		return 0
	}
	fmt.Println("RESULT: NOT EQUIVALENT")
	return 1
}
