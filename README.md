# mev-sim

Research-grade historical simulator for Ethereum MEV, starting with
flash-loan-assisted atomic DEX arbitrage. Correctness over demos.

```
make test        # all unit tests
make build       # bin/mevsim
./bin/mevsim status
./bin/mevsim evaluate --scenario examples/synthetic_two_pool.json
```

- Architecture, data model, data sources: `docs/ARCHITECTURE.md`, `docs/schema.sql`
- How correctness is proven: `docs/VALIDATION.md`
- Phases and current gaps: `docs/ROADMAP.md`

**Current phase: 1 (math core).** No historical results exist yet; the
example scenario is labelled SYNTHETIC and says nothing about real profit.
