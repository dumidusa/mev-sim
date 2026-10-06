package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/dumidu/mev-sim/internal/eth/ethtest"
	"github.com/dumidu/mev-sim/internal/ingest"
)

// devnet: the deterministic fake chain used by the tests, served on
// localhost. SYNTHETIC data - for trying the pipeline, never for results.
const devnetChainID = 31337

func devnetConfig() ethtest.GenConfig {
	c := ethtest.DefaultConfig()
	c.ChainID, c.NumBlocks = devnetChainID, 400
	return c
}

func devnetFactories() []ingest.Factory {
	c := devnetConfig()
	var out []ingest.Factory
	for i, f := range c.Factories {
		out = append(out, ingest.Factory{Name: fmt.Sprintf("devnet_factory_%d", i+1), Address: f.Address, FeeBps: f.FeeBps, DeployBlock: c.FirstBlock})
	}
	return out
}

func cmdDevnet(args []string) int {
	fs := flag.NewFlagSet("devnet", flag.ExitOnError)
	port := fs.Int("port", 8545, "listen port")
	fs.Parse(args)
	cfg := devnetConfig()
	ch := ethtest.Generate(cfg)
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	fmt.Printf("SYNTHETIC devnet (chain id %d) at http://%s\n", devnetChainID, addr)
	fmt.Printf("blocks %d → %d, %d pools. Try:\n", cfg.FirstBlock, ch.Head(), len(ch.PoolAddresses()))
	fmt.Printf("  mevsim ingest --rpc http://%s --factories devnet --db data/devnet.db --start-block %d --end-block %d --confirmations 10\n",
		addr, cfg.FirstBlock+10, ch.Head()-10)
	fmt.Printf("  mevsim validate --rpc http://%s --db data/devnet.db\n", addr)
	log.Fatal(http.ListenAndServe(addr, ch.Handler()))
	return 0
}
