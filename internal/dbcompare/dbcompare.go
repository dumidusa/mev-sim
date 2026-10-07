// Package dbcompare checks that two ingested databases hold the same data,
// table by table and column by column (e.g. RPC-ingested vs BigQuery-ingested).
package dbcompare

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

type spec struct {
	table, key, blockCol string // blockCol "" = no range filter
}

var tables = []spec{
	{"meta", "key", ""},
	{"factories", "address", ""},
	{"pools", "address", ""},
	{"probes", "address", ""},
	{"v2_seed", "pool", ""},
	{"v2_sync", "block_number, log_index", "block_number"},
	{"v2_swap", "block_number, log_index", "block_number"},
	{"blocks", "number", "number"},
	{"transactions", "block_number, tx_index", "block_number"},
	{"unknown_emitters", "address, topic", ""},
}

type TableDiff struct {
	Table        string
	RowsA, RowsB int
	OnlyA, OnlyB int
	RowsDiffer   int
	Cols         map[string]int // column -> rows where it differs
	Examples     []string
}

func (d TableDiff) Clean(ignore map[string]bool) bool {
	if d.OnlyA > 0 || d.OnlyB > 0 {
		return false
	}
	for c, n := range d.Cols {
		if n > 0 && !ignore[d.Table+"."+c] {
			return false
		}
	}
	return true
}

func load(db *sql.DB, s spec, from, to uint64) (map[string]map[string]string, []string, error) {
	q := "SELECT * FROM " + s.table
	var args []any
	if s.blockCol != "" {
		q += " WHERE " + s.blockCol + " BETWEEN ? AND ?"
		args = []any{from, to}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	keys := strings.Split(strings.ReplaceAll(s.key, " ", ""), ",")
	out := map[string]map[string]string{}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		r := map[string]string{}
		for i, c := range cols {
			if vals[i].Valid {
				r[c] = vals[i].String
			} else {
				r[c] = "NULL"
			}
		}
		var k []string
		for _, kc := range keys {
			k = append(k, r[kc])
		}
		out[strings.Join(k, "|")] = r
	}
	return out, cols, rows.Err()
}

// Compare a and b over [from, to] (block-keyed tables only; others whole).
func Compare(a, b *sql.DB, from, to uint64) ([]TableDiff, error) {
	var res []TableDiff
	for _, s := range tables {
		ra, cols, err := load(a, s, from, to)
		if err != nil {
			return nil, fmt.Errorf("%s (a): %w", s.table, err)
		}
		rb, _, err := load(b, s, from, to)
		if err != nil {
			return nil, fmt.Errorf("%s (b): %w", s.table, err)
		}
		d := TableDiff{Table: s.table, RowsA: len(ra), RowsB: len(rb), Cols: map[string]int{}}
		keys := make([]string, 0, len(ra))
		for k := range ra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			x := ra[k]
			y, ok := rb[k]
			if !ok {
				d.OnlyA++
				if len(d.Examples) < 3 {
					d.Examples = append(d.Examples, "only in A: "+k)
				}
				continue
			}
			diff := false
			for _, c := range cols {
				if x[c] != y[c] {
					d.Cols[c]++
					diff = true
					if len(d.Examples) < 3 {
						d.Examples = append(d.Examples, fmt.Sprintf("%s.%s @%s: A=%s B=%s", s.table, c, k, x[c], y[c]))
					}
				}
			}
			if diff {
				d.RowsDiffer++
			}
		}
		for k := range rb {
			if _, ok := ra[k]; !ok {
				d.OnlyB++
				if len(d.Examples) < 3 {
					d.Examples = append(d.Examples, "only in B: "+k)
				}
			}
		}
		res = append(res, d)
	}
	return res, nil
}
