# CLOB-Aware Mempool: Implementation Guide

This document describes the implementation details of the CLOB-aware mempool
feature introduced in ADR-010. It serves as a reference for developers
maintaining or extending this code.

## File Layout

| File | Purpose |
|------|---------|
| `app/clob_mempool.go` | `CLOBMempool` type + `isCLOBTx` helper |
| `app/proposal.go` | `CLOBTxSelector` (added alongside existing `ExtTxSelector`) |
| `app/app.go` | Three-way mempool init + conditional selector wiring |
| `cmd/cronosd/config/config.go` | `CLOBGasRatio` config field |
| `cmd/cronosd/config/toml.go` | TOML template entry |
| `app/clob_mempool_test.go` | CLOBMempool unit tests + shared test helpers |
| `app/proposal_test.go` | CLOBTxSelector unit tests |
| `app/clob_integration_test.go` | End-to-end PrepareProposal integration tests |

## CLOBMempool (`app/clob_mempool.go`)

### CLOB Transaction Detection

```go
func isCLOBTx(tx sdk.Tx) bool {
    for _, msg := range tx.GetMsgs() {
        if _, ok := msg.(*exchangetypes.MsgSettleBatch); ok {
            return true
        }
    }
    return false
}
```

A transaction is classified as CLOB if it contains at least one
`MsgSettleBatch`. This check runs at insertion time (`Insert` /
`InsertWithGasWanted`) and again in the `CLOBTxSelector` during proposal
building.

### Dual-Pool Structure

```go
type CLOBMempool struct {
    clobPool    *mempool.PriorityNonceMempool[int64]
    regularPool *mempool.PriorityNonceMempool[int64]
}
```

Both pools share the same configuration (`TxPriority`, `SignerExtractor`,
`MaxTx`, `TxReplacement`). Each pool operates independently with its own
locks, sender indices, and priority indices.

### Method Routing

| Method | Routing Logic |
|--------|--------------|
| `Insert` / `InsertWithGasWanted` | `isCLOBTx(tx)` → `clobPool`, else → `regularPool` |
| `Remove` | Try `clobPool` first; on `ErrTxNotFound`, fall through to `regularPool` |
| `CountTx` | Sum of both pools |
| `Select` | `chainedIterator(clobPool.Select(), regularPool.Select())` |
| `SelectBy` | Iterate `clobPool.SelectBy` first; if callback keeps returning `true`, continue with `regularPool.SelectBy` |

### `chainedIterator`

The `Select` method returns a `chainedIterator` that exhausts the first
iterator before advancing to the second. If `clobPool` is empty, the
regular pool's iterator is returned directly (no wrapping overhead).

## CLOBTxSelector (`app/proposal.go`)

### State Fields

```go
type CLOBTxSelector struct {
    clobGasRatio   float64       // configured ratio [0.0, 1.0]
    clobGasLimit   uint64        // clobGasRatio × maxBlockGas (lazy-init)
    clobGasUsed    uint64        // running total of accepted CLOB gas
    maxBlockGas    uint64        // from ConsensusParams (lazy-init)
    regularGasUsed uint64        // running total of accepted regular gas
    totalTxBytes   uint64        // running total of protobuf-encoded tx bytes
    selectedTxs    [][]byte      // accepted tx bytes for the proposal
    initialized    bool          // lazy-init flag (reset by Clear)
    isCLOBTx       func(sdk.Tx) bool
    validateTx     func(sdk.Tx, []byte) error
    txDecoder      sdk.TxDecoder
}
```

### `SelectTxForProposal` Flow

```
1. Decode tx if memTx is nil (guard: skip if decoder returns nil)
2. Validate tx via validateTx (blocklist) → skip on error (return false)
3. Lazy-init clobGasLimit and maxBlockGas on first call
4. Compute txSize via ComputeProtoSizeForTxs
5. Check byte budget: txSize + totalTxBytes > maxTxBytes → halt (return true)
6. Gas checks (skipped when maxBlockGas = 0):
   a. CLOB tx: clobGasUsed + gasWanted > clobGasLimit → skip (return false)
              else: clobGasUsed += gasWanted
   b. Regular tx: effectiveRegularLimit = maxBlockGas − clobGasUsed
                  regularGasUsed + gasWanted > effectiveRegularLimit → halt (return true)
                  else: regularGasUsed += gasWanted
7. Add tx: totalTxBytes += txSize, append txBz
8. Return: totalTxBytes >= maxTxBytes || totalGas >= maxBlockGas
```

### Return Value Semantics

| Return | Meaning | When |
|--------|---------|------|
| `false` | Continue iterating | Tx accepted, or tx skipped (CLOB over quota, validation error) |
| `true` | Stop iterating | Byte budget exhausted, regular gas limit hit, or block full |

The distinction matters: skipping a CLOB tx returns `false` (continue) so
smaller CLOB txs or regular txs can still be selected. Exceeding the regular
gas limit returns `true` (halt) because no further regular txs can fit.

### `Clear`

Called via `defer` at the end of each `PrepareProposalHandler` invocation.
Resets all counters and sets `initialized = false` so the next block
recomputes limits from the fresh `maxBlockGas` in `ConsensusParams`.

## Wiring in `app/app.go`

### Config Reading

```go
clobGasRatio := cast.ToFloat64(appOpts.Get(FlagCLOBGasRatio))
if clobGasRatio > 1.0 {
    clobGasRatio = 1.0  // clamp to prevent uint64 underflow
}
```

### Three-Way Mempool Init

```go
if mempoolMaxTxs >= 0 && feeBump >= 0 && clobGasRatio > 0 {
    mpool = NewCLOBMempool(...)
} else if mempoolMaxTxs >= 0 && feeBump >= 0 {
    mpool = mempool.NewPriorityMempool(...)  // existing path
} else {
    mpool = mempool.NoOpMempool{}            // existing path
}
```

### Selector Wiring

```go
if _, ok := mpool.(*CLOBMempool); ok {
    defaultProposalHandler.SetTxSelector(NewCLOBTxSelector(...))
} else {
    defaultProposalHandler.SetTxSelector(NewExtTxSelector(...))
}
```

The type assertion ensures the selector and mempool are always consistent,
even if config parameters create unexpected combinations.

## Configuration

### `app.toml`

```toml
[cronos]
# Fraction of block gas reserved for CLOB (MsgSettleBatch) transactions [0.0 to 1.0].
# CLOB transactions are placed first in every block.
# Unused CLOB gas quota rolls over to regular EVM transactions.
# Set to 0.0 to disable (default).
clob-gas-ratio = 0.0
```

### Recommended Values

| Scenario | Ratio | Effect |
|----------|-------|--------|
| CLOB disabled (default) | `0.0` | No CLOBMempool; uses standard PriorityNonceMempool |
| Light CLOB usage | `0.1` | 10% of block gas reserved for settlement batches |
| Standard CLOB usage | `0.3` | 30% reserved; recommended starting point |
| Heavy CLOB usage | `0.5` | 50% reserved; use when settlement volume is high |
| CLOB-only chain | `1.0` | 100% reserved; regular txs only get unused CLOB capacity |

## Test Coverage

### Unit Tests — CLOBMempool (`app/clob_mempool_test.go`)

| Test | Scenario |
|------|----------|
| `TestCLOBMempoolRouting` | CLOB vs regular tx routing to correct sub-pool |
| `TestCLOBMempoolInsertWithGasWanted` | InsertWithGasWanted routes correctly |
| `TestCLOBMempoolRemove` | Remove from both pools |
| `TestCLOBMempoolSelectByOrder` | SelectBy returns CLOB txs before regular txs |
| `TestCLOBMempoolSelectByHalt` | Callback returning false halts iteration |
| `TestCLOBMempoolSelectIterator` | Select (iterator-based) returns CLOB first |
| `TestCLOBMempoolRemoveNotFound` | Remove on empty pool returns ErrTxNotFound |
| `TestCLOBMempoolMaxTxCapacity` | MaxTx=1 returns ErrMempoolTxMaxCapacity |
| `TestCLOBMempoolEmptyCLOBPool` | Empty clobPool: only regular txs iterated |
| `TestCLOBMempoolMultipleCLOBSigners` | Multiple CLOB signers all route to clobPool |

### Unit Tests — CLOBTxSelector (`app/proposal_test.go`)

| Test | Scenario |
|------|----------|
| `TestCLOBTxSelectorCLOBGasLimit` | CLOB tx exceeding quota is skipped (not halted) |
| `TestCLOBTxSelectorRollover` | No CLOB txs: full block gas available to regular |
| `TestCLOBTxSelectorMixed` | CLOB + regular txs filling block to exactly maxBlockGas |
| `TestCLOBTxSelectorRegularGasExceeded` | Regular tx exceeding effective limit halts |
| `TestCLOBTxSelectorUnlimitedGas` | maxBlockGas=0: gas checks bypassed |
| `TestCLOBTxSelectorValidationError` | Blocklist validation rejects tx (skip, not halt) |
| `TestCLOBTxSelectorClear` | Clear resets all counters and initialized flag |
| `TestCLOBTxSelectorBytesLimit` | tx exceeding byte budget halts |
| `TestCLOBTxSelectorCLOBRatioFull` | ratio=1.0: regular txs only get rollover |
| `TestCLOBTxSelectorClearAndReuse` | Clear + re-init with different maxBlockGas |

### Integration Tests (`app/clob_integration_test.go`)

These wire `CLOBMempool` + `CLOBTxSelector` + `baseapp.DefaultProposalHandler`
end-to-end using a `mockProposalVerifier`:

| Test | Scenario |
|------|----------|
| `TestCLOBIntegration_OrderCLOBFirst` | CLOB tx inserted after regular tx still appears first |
| `TestCLOBIntegration_CLOBGasExceedsQuota` | Oversized CLOB tx excluded; regular tx included |
| `TestCLOBIntegration_GasRollover` | No CLOB txs: 9M regular tx accepted (full 10M rollover) |
| `TestCLOBIntegration_PartialCLOBQuota` | 2M CLOB + 7M regular both selected; CLOB first |
| `TestCLOBIntegration_UnlimitedGas` | maxBlockGas=0: all txs included |

### Test Helpers

Shared test infrastructure in `app/clob_mempool_test.go`:

- `testTx` — minimal `sdk.Tx` with `GetMsgs()` and `GetMsgsV2()`
- `testSignerExtractor` — reads signer/nonce directly from `*testTx`
- `newTestSDKCtx()` — creates `sdk.Context` with nil multistore (safe for priority extraction)
- `newCLOBTx()` / `newRegularTx()` — create test txs with/without `MsgSettleBatch`
- `mockProposalVerifier` — bidirectional tx-to-bytes map implementing `baseapp.ProposalTxVerifier`

## Extending the Feature

### Adding New CLOB Message Types

Update `isCLOBTx` in `app/clob_mempool.go`:

```go
func isCLOBTx(tx sdk.Tx) bool {
    for _, msg := range tx.GetMsgs() {
        switch msg.(type) {
        case *exchangetypes.MsgSettleBatch:
            return true
        case *exchangetypes.MsgNewOrderType:  // hypothetical
            return true
        }
    }
    return false
}
```

### Per-Type Gas Limits

The current design has a single CLOB gas quota. To support multiple
categories (e.g., CLOB + DA), extend `CLOBTxSelector` with additional
gas counters and detection functions. The rollover formula generalizes to:

```
effectiveRegularLimit = maxBlockGas − sum(categoryGasUsed)
```

### Separate MaxTx per Pool

Currently both pools share the same `MaxTx`. To allow independent limits,
change `NewCLOBMempool` to accept separate `clobMaxTx` and `regularMaxTx`
parameters and build two different configs.
