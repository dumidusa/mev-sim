package eth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// RPCError is a JSON-RPC error returned by the node.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// IsLogLimit reports whether err is a provider "range/result too large" error.
// Providers word this differently; we match the common forms.
func IsLogLimit(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) {
		return false
	}
	m := strings.ToLower(re.Message)
	for _, k := range []string{"range", "too many", "limit", "exceed", "10000", "response size", "query returned more than"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return re.Code == -32005
}

type Client struct {
	URL        string
	HTTP       *http.Client
	MaxRetries int
	id         atomic.Uint64
}

func NewClient(url string) *Client {
	return &Client{URL: url, HTTP: &http.Client{Timeout: 60 * time.Second}, MaxRetries: 5}
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Call performs one JSON-RPC call with retry on transport errors, HTTP 429
// and 5xx. Node-level errors (RPCError) are NOT retried: they are returned
// so the caller can react (e.g. split a log range).
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(request{"2.0", c.id.Add(1), method, params})
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(200<<attempt) * time.Millisecond):
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: http %d", method, resp.StatusCode)
			continue
		}
		var r response
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: decode response: %w (http %d)", method, err, resp.StatusCode)
		}
		if r.Error != nil {
			return r.Error
		}
		if string(r.Result) == "null" {
			return fmt.Errorf("%s: null result", method)
		}
		return json.Unmarshal(r.Result, out)
	}
	return fmt.Errorf("%s: retries exhausted: %w", method, lastErr)
}

// ---- wire formats ------------------------------------------------------

type rawLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	BlockHash   string   `json:"blockHash"`
	TxHash      string   `json:"transactionHash"`
	TxIndex     string   `json:"transactionIndex"`
	LogIndex    string   `json:"logIndex"`
	Removed     bool     `json:"removed"`
}

func (r rawLog) parse() (Log, error) {
	var l Log
	var err error
	l.Address = strings.ToLower(r.Address)
	for _, t := range r.Topics {
		l.Topics = append(l.Topics, strings.ToLower(t))
	}
	if l.Data, err = ParseBytes(r.Data); err != nil {
		return l, err
	}
	if l.BlockNumber, err = ParseQty(r.BlockNumber); err != nil {
		return l, err
	}
	if l.TxIndex, err = ParseQty(r.TxIndex); err != nil {
		return l, err
	}
	if l.LogIndex, err = ParseQty(r.LogIndex); err != nil {
		return l, err
	}
	l.BlockHash, l.TxHash, l.Removed = strings.ToLower(r.BlockHash), strings.ToLower(r.TxHash), r.Removed
	return l, nil
}

type rawTx struct {
	Hash        string `json:"hash"`
	Index       string `json:"transactionIndex"`
	From        string `json:"from"`
	To          string `json:"to"`
	Nonce       string `json:"nonce"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	Gas         string `json:"gas"`
	GasPrice    string `json:"gasPrice"`
	MaxFee      string `json:"maxFeePerGas"`
	MaxPriority string `json:"maxPriorityFeePerGas"`
	Input       string `json:"input"`
}

type rawBlock struct {
	Number     string  `json:"number"`
	Hash       string  `json:"hash"`
	ParentHash string  `json:"parentHash"`
	Timestamp  string  `json:"timestamp"`
	BaseFee    string  `json:"baseFeePerGas"`
	GasLimit   string  `json:"gasLimit"`
	GasUsed    string  `json:"gasUsed"`
	Miner      string  `json:"miner"`
	Txs        []rawTx `json:"transactions"`
}

type rawReceipt struct {
	TxHash            string   `json:"transactionHash"`
	TxIndex           string   `json:"transactionIndex"`
	Status            string   `json:"status"`
	GasUsed           string   `json:"gasUsed"`
	EffectiveGasPrice string   `json:"effectiveGasPrice"`
	Logs              []rawLog `json:"logs"`
}

// first error wins helper
type parser struct{ err error }

func (p *parser) qty(s string) uint64 {
	if p.err != nil || s == "" {
		return 0
	}
	v, err := ParseQty(s)
	p.err = err
	return v
}
func (p *parser) big(s string) *big.Int {
	if p.err != nil {
		return nil
	}
	v, err := ParseBig(s)
	p.err = err
	return v
}

// ---- Chain implementation ----------------------------------------------

func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var s string
	if err := c.Call(ctx, "eth_chainId", nil, &s); err != nil {
		return 0, err
	}
	return ParseQty(s)
}

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var s string
	if err := c.Call(ctx, "eth_blockNumber", nil, &s); err != nil {
		return 0, err
	}
	return ParseQty(s)
}

func (c *Client) BlockByNumber(ctx context.Context, n uint64) (*Block, error) {
	var rb rawBlock
	if err := c.Call(ctx, "eth_getBlockByNumber", []any{Qty(n), true}, &rb); err != nil {
		return nil, err
	}
	p := &parser{}
	b := &Block{
		Number: p.qty(rb.Number), Hash: strings.ToLower(rb.Hash), ParentHash: strings.ToLower(rb.ParentHash),
		Timestamp: p.qty(rb.Timestamp), GasLimit: p.qty(rb.GasLimit), GasUsed: p.qty(rb.GasUsed),
		Miner: strings.ToLower(rb.Miner),
	}
	b.BaseFee = p.big(rb.BaseFee)
	for _, rt := range rb.Txs {
		tx := Tx{
			Hash: strings.ToLower(rt.Hash), Index: p.qty(rt.Index), From: strings.ToLower(rt.From),
			To: strings.ToLower(rt.To), Nonce: p.qty(rt.Nonce), Type: p.qty(rt.Type), GasLimit: p.qty(rt.Gas),
		}
		tx.Value = p.big(rt.Value)
		tx.GasPrice = p.big(rt.GasPrice)
		tx.MaxFee = p.big(rt.MaxFee)
		tx.MaxPriority = p.big(rt.MaxPriority)
		if len(rt.Input) >= 10 {
			tx.Selector = strings.ToLower(rt.Input[:10])
		}
		b.Txs = append(b.Txs, tx)
	}
	if p.err != nil {
		return nil, fmt.Errorf("block %d: %w", n, p.err)
	}
	if b.Number != n {
		return nil, fmt.Errorf("asked block %d, node returned %d", n, b.Number)
	}
	return b, nil
}

func (c *Client) BlockReceipts(ctx context.Context, n uint64) ([]Receipt, error) {
	var rr []rawReceipt
	if err := c.Call(ctx, "eth_getBlockReceipts", []any{Qty(n)}, &rr); err != nil {
		return nil, err
	}
	p := &parser{}
	out := make([]Receipt, 0, len(rr))
	for _, r := range rr {
		rc := Receipt{TxHash: strings.ToLower(r.TxHash), TxIndex: p.qty(r.TxIndex), Status: p.qty(r.Status), GasUsed: p.qty(r.GasUsed)}
		rc.EffectiveGasPrice = p.big(r.EffectiveGasPrice)
		for _, rl := range r.Logs {
			l, err := rl.parse()
			if err != nil && p.err == nil {
				p.err = err
			}
			rc.Logs = append(rc.Logs, l)
		}
		out = append(out, rc)
	}
	if p.err != nil {
		return nil, fmt.Errorf("receipts %d: %w", n, p.err)
	}
	return out, nil
}

func (c *Client) GetLogs(ctx context.Context, f LogFilter) ([]Log, error) {
	q := map[string]any{"fromBlock": Qty(f.FromBlock), "toBlock": Qty(f.ToBlock)}
	if len(f.Addresses) > 0 {
		q["address"] = f.Addresses
	}
	if len(f.Topics) > 0 {
		ts := make([]any, len(f.Topics))
		for i, t := range f.Topics {
			if len(t) == 0 {
				ts[i] = nil
			} else {
				ts[i] = t
			}
		}
		q["topics"] = ts
	}
	var rl []rawLog
	if err := c.Call(ctx, "eth_getLogs", []any{q}, &rl); err != nil {
		return nil, err
	}
	out := make([]Log, 0, len(rl))
	for _, r := range rl {
		l, err := r.parse()
		if err != nil {
			return nil, err
		}
		if l.Removed {
			return nil, fmt.Errorf("log %s#%d marked removed (reorg) - range not final", l.TxHash, l.LogIndex)
		}
		out = append(out, l)
	}
	return out, nil
}

func (c *Client) CallAt(ctx context.Context, to string, data []byte, block uint64) ([]byte, error) {
	var s string
	if err := c.Call(ctx, "eth_call", []any{map[string]any{"to": to, "data": Hex(data)}, Qty(block)}, &s); err != nil {
		return nil, err
	}
	return ParseBytes(s)
}
