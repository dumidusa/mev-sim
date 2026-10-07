package scan

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"os"

	"github.com/dumidu/mev-sim/internal/optimize"
)

// Candidates are written to a temporary file as soon as they are final, so
// memory does not grow with the number of candidate states (millions over a
// week). Only candidates an episode still points at stay in memory.
//
// The file is the full record: Save, ResultsHash and Result.Each read it back
// in candidate-id order.

type curveRec struct {
	L, Profit *big.Int
	Err       string
}

type candRec struct {
	C      Candidate // Curve is moved to CurveR (error values are not gob-able)
	CurveR []curveRec
}

type spill struct {
	path string
	f    *os.File
	bw   *bufio.Writer
	zw   *gzip.Writer
	enc  *gob.Encoder
	n    int
	h    hash.Hash // canonical hash of all candidate rows, in id order
}

func newSpill(dir string) (*spill, error) {
	f, err := os.CreateTemp(dir, "mevsim-cands-*.gob.gz")
	if err != nil {
		return nil, fmt.Errorf("candidate spill file: %w", err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	zw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	return &spill{path: f.Name(), f: f, bw: bw, zw: zw, enc: gob.NewEncoder(zw), h: sha256.New()}, nil
}

func (s *spill) write(c *Candidate, routeID string) error {
	if c.ID != s.n {
		return fmt.Errorf("spill: candidate id %d written out of order (want %d)", c.ID, s.n)
	}
	hashRow(s.h, "c", c.ID, routeID, c.Point, c.TriggerTx, c.Fingerprint, c.Stage, c.Reason,
		sv(c.L), sv(c.Out), sv(c.Gross), sv(c.FlashFee), c.GasUsed, sv(c.BaseFee), sv(c.Priority), sv(c.GasCost),
		sv(c.Builder), sv(c.Net), ratStr(c.NetUSD), c.Episode)
	rec := candRec{C: *c}
	rec.C.Curve = nil
	for _, p := range c.Curve {
		e := ""
		if p.Err != nil {
			e = p.Err.Error()
		}
		rec.CurveR = append(rec.CurveR, curveRec{p.L, p.Profit, e})
	}
	s.n++
	return s.enc.Encode(&rec)
}

func (s *spill) sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

func hashRow(h io.Writer, parts ...any) {
	for _, p := range parts {
		fmt.Fprint(h, p, "\x1f")
	}
	fmt.Fprint(h, "\x1e")
}

func sv(b *big.Int) string {
	if b == nil {
		return "-"
	}
	return b.String()
}

// close finishes writing; the file stays until Result.Close.
func (s *spill) close() error {
	if s.f == nil {
		return nil
	}
	err := errors.Join(s.zw.Close(), s.bw.Flush(), s.f.Close())
	s.f = nil
	return err
}

// Each calls fn for every candidate in id order (read back from disk).
func (r *Result) Each(fn func(c *Candidate) error) error {
	if r.spill == nil {
		for _, c := range r.Kept { // results built in memory (unit tests)
			if c != nil {
				if err := fn(c); err != nil {
					return err
				}
			}
		}
		return nil
	}
	f, err := os.Open(r.spill.path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(zr)
	for i := 0; ; i++ {
		var rec candRec
		if err := dec.Decode(&rec); err == io.EOF {
			if i != r.NCands {
				return fmt.Errorf("spill: read %d of %d candidates", i, r.NCands)
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("spill read: %w", err)
		}
		c := rec.C
		for _, p := range rec.CurveR {
			var e error
			if p.Err != "" {
				e = errors.New(p.Err)
			}
			c.Curve = append(c.Curve, optimize.Point{L: p.L, Profit: p.Profit, Err: e})
		}
		if err := fn(&c); err != nil {
			return err
		}
	}
}

// Close removes the temporary candidate file.
func (r *Result) Close() error {
	if r.spill == nil {
		return nil
	}
	r.spill.close()
	err := os.Remove(r.spill.path)
	r.spill = nil
	return err
}
