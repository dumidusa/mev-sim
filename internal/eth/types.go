// Package eth is a small, dependency-free Ethereum JSON-RPC layer.
// Only what ingestion needs: blocks, receipts, logs, eth_call.
package eth

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type Log struct {
	Address     string // lowercase
	Topics      []string
	Data        []byte
	BlockNumber uint64
	BlockHash   string
	TxHash      string
	TxIndex     uint64
	LogIndex    uint64
	Removed     bool
}

type Tx struct {
	Hash        string
	Index       uint64
	From, To    string // To = "" for contract creation
	Nonce       uint64
	Type        uint64
	Value       *big.Int
	GasLimit    uint64
	GasPrice    *big.Int // legacy, or effective in some node outputs
	MaxFee      *big.Int // nil for legacy
	MaxPriority *big.Int // nil for legacy
	Selector    string   // first 4 bytes of input, "" if none
}

type Receipt struct {
	TxHash            string
	TxIndex           uint64
	Status            uint64
	GasUsed           uint64
	EffectiveGasPrice *big.Int
	Logs              []Log
}

type Block struct {
	Number     uint64
	Hash       string
	ParentHash string
	Timestamp  uint64
	BaseFee    *big.Int // nil before London
	GasLimit   uint64
	GasUsed    uint64
	Miner      string
	Txs        []Tx
}

type LogFilter struct {
	FromBlock, ToBlock uint64
	Addresses          []string   // empty = any
	Topics             [][]string // topics[0] = OR-set for topic0, ...
}

// Chain is everything ingestion needs from a node.
type Chain interface {
	ChainID(ctx context.Context) (uint64, error)
	BlockNumber(ctx context.Context) (uint64, error)
	BlockByNumber(ctx context.Context, n uint64) (*Block, error)
	BlockReceipts(ctx context.Context, n uint64) ([]Receipt, error)
	GetLogs(ctx context.Context, f LogFilter) ([]Log, error)
	CallAt(ctx context.Context, to string, data []byte, block uint64) ([]byte, error)
}

// ---- hex helpers -------------------------------------------------------

func Qty(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func ParseQty(s string) (uint64, error) {
	if !strings.HasPrefix(s, "0x") {
		return 0, fmt.Errorf("quantity %q: missing 0x", s)
	}
	return strconv.ParseUint(s[2:], 16, 64)
}

func ParseBig(s string) (*big.Int, error) {
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "0x") {
		return nil, fmt.Errorf("big %q: missing 0x", s)
	}
	if s == "0x" {
		return new(big.Int), nil
	}
	v, ok := new(big.Int).SetString(s[2:], 16)
	if !ok {
		return nil, fmt.Errorf("big %q: bad hex", s)
	}
	return v, nil
}

func ParseBytes(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "0x") {
		return nil, fmt.Errorf("bytes %q: missing 0x", s)
	}
	return hex.DecodeString(s[2:])
}

func Hex(b []byte) string { return "0x" + hex.EncodeToString(b) }
