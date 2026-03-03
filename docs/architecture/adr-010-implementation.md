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
| `cmd/cronosd/config/config.go` | `CLOBBlockRatio` config field |
| `cmd/cronosd/config/toml.go` | TOML template entry |
| `app/clob_mempool_test.go` | CLOBMempool unit tests + shared test helpers |
| `app/proposal_test.go` | CLOBTxSelector unit tests |
| `app/clob_integration_test.go` | End-to-end PrepareProposal integration tests |

## CLOBMempool (`app/clob_mempool.go`)

### CLOB Transaction Detection

```go
func NewCLOBTxDetector(sequencerAddr sdk.AccAddress) func(sdk.Tx) bool {
    if len(sequencerAddr) == 0 {
        return func(sdk.Tx) bool { return false }
    }
    return func(tx sdk.Tx) bool {
        for _, msg := range tx.GetMsgs() {
            ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
            if ok && bytes.Equal(ethMsg.GetFrom(), sequencerAddr) {
                return true
            }
        }
        return false
    }
}
```

A transaction is classified as CLOB if it contains a `MsgEthereumTx` whose
sender (via `GetFrom()`) matches the configured sequencer address. The detector
is a closure created at startup via `NewCLOBTxDetector(sequencerAddr)` and
passed to both `CLOBMempool` and `CLOBTxSelector`. If no sequencer address is
configured, the detector always returns false (all txs go to regularPool).

This check runs at insertion time (`Insert` / `InsertWithGasWanted`) and again
in the `CLOBTxSelector` during proposal building.

### Dual-Pool Structure

```go
type CLOBMempool struct {
    clobPool    *mempool.PriorityNonceMempool[int64]
    regularPool *mempool.PriorityNonceMempool[int64]
    isCLOBTx    func(sdk.Tx) bool
}
```

Both pools share the same configuration (`TxPriority`, `SignerExtractor`,
`MaxTx`, `TxReplacement`). Each pool operates independently with its own
locks, sender indices, and priority indices.

### Method Routing

| Method | Routing Logic |
|--------|--------------|
| `Insert` / `InsertWithGasWanted` | `cm.isCLOBTx(tx)` → `clobPool`, else → `regularPool` |
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
    clobBlockRatio   float64       // configured ratio [0.0, 1.0]
    clobGasLimit     uint64        // clobBlockRatio × maxBlockGas (lazy-init)
    clobGasUsed      uint64        // running total of accepted CLOB gas
    maxBlockGas      uint64        // from ConsensusParams (lazy-init)
    regularGasUsed   uint64        // running total of accepted regular gas
    clobBytesLimit   uint64        // clobBlockRatio × maxTxBytes (lazy-init)
    clobBytesUsed    uint64        // running total of accepted CLOB tx bytes
    regularBytesUsed uint64        // running total of accepted regular tx bytes
    maxTxBytes       uint64        // from PrepareProposal request (lazy-init)
    selectedTxs      [][]byte      // accepted tx bytes for the proposal
    initialized      bool          // lazy-init flag (reset by Clear)
    isCLOBTx         func(sdk.Tx) bool
    validateTx       func(sdk.Tx, []byte) error
    txDecoder        sdk.TxDecoder
}
```

### `SelectTxForProposal` Flow

```
1. Decode tx if memTx is nil (guard: skip if decoder returns nil)
2. Validate tx via validateTx (blocklist) → skip on error (return false)
3. Lazy-init clobGasLimit, clobBytesLimit, maxBlockGas, maxTxBytes on first call
4. Compute txSize via ComputeProtoSizeForTxs
5. If CLOB tx:
   a. clobBytesUsed + txSize > clobBytesLimit → skip (return false)
   b. clobGasUsed + gasWanted > clobGasLimit (when maxBlockGas > 0) → skip (return false)
   c. Accept: clobBytesUsed += txSize, clobGasUsed += gasWanted
6. If regular tx:
   a. effectiveRegularBytesLimit = maxTxBytes − clobBytesUsed
      regularBytesUsed + txSize > effectiveRegularBytesLimit → halt (return true)
   b. effectiveRegularGasLimit = maxBlockGas − clobGasUsed (when maxBlockGas > 0)
      regularGasUsed + gasWanted > effectiveRegularGasLimit → halt (return true)
   c. Accept: regularBytesUsed += txSize, regularGasUsed += gasWanted
7. Append txBz to selectedTxs
8. Return: totalBytes >= maxTxBytes || totalGas >= maxBlockGas
```

### Return Value Semantics

| Return | Meaning | When |
|--------|---------|------|
| `false` | Continue iterating | Tx accepted, or tx skipped (CLOB over gas/byte quota, validation error) |
| `true` | Stop iterating | Regular byte/gas limit hit, or block full |

The distinction matters: skipping a CLOB tx returns `false` (continue) so
smaller CLOB txs or regular txs can still be selected. Exceeding the regular
gas or byte limit returns `true` (halt) because no further regular txs can fit.

### `Clear`

Called via `defer` at the end of each `PrepareProposalHandler` invocation.
Resets all counters (gas and byte) and sets `initialized = false` so the next
block recomputes limits from the fresh `maxBlockGas` and `maxTxBytes`.

## Wiring in `app/app.go`

### Config Reading

```go
clobBlockRatio := cast.ToFloat64(appOpts.Get(FlagCLOBBlockRatio))
if clobBlockRatio > 1.0 {
    clobBlockRatio = 1.0  // clamp to prevent uint64 underflow
}
clobSequencerAddress := cast.ToString(appOpts.Get(FlagCLOBSequencerAddress))
var sequencerAddr sdk.AccAddress
if clobSequencerAddress != "" {
    sequencerAddr, err = sdk.AccAddressFromBech32(clobSequencerAddress)
    // panics on invalid address
}
isCLOBFn := NewCLOBTxDetector(sequencerAddr)
```

### Three-Way Mempool Init

```go
if mempoolMaxTxs >= 0 && feeBump >= 0 && clobBlockRatio > 0 && len(sequencerAddr) > 0 {
    mpool = NewCLOBMempool(..., isCLOBFn)
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
# Fraction of block resources (gas and bytes) reserved for CLOB transactions [0.0 to 1.0].
# CLOB transactions are placed first in every block.
# Unused CLOB quota rolls over to regular EVM transactions.
# Set to 0.0 to disable (default).
clob-block-ratio = 0.0

# Bech32 address of the off-chain CLOB sequencer.
# MsgEthereumTx transactions from this sender are classified as CLOB.
# Leave empty to disable CLOB detection (default).
clob-sequencer-address = ""
```

### Recommended Values

| Scenario | Ratio | Effect |
|----------|-------|--------|
| CLOB disabled (default) | `0.0` | No CLOBMempool; uses standard PriorityNonceMempool |
| Light CLOB usage | `0.1` | 10% of block gas+bytes reserved for settlement batches |
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
| `TestCLOBTxSelectorCLOBGasLimit` | CLOB tx exceeding gas quota is skipped (not halted) |
| `TestCLOBTxSelectorRollover` | No CLOB txs: full block gas available to regular |
| `TestCLOBTxSelectorMixed` | CLOB + regular txs filling block to exactly maxBlockGas |
| `TestCLOBTxSelectorRegularGasExceeded` | Regular tx exceeding effective gas limit halts |
| `TestCLOBTxSelectorUnlimitedGas` | maxBlockGas=0: gas checks bypassed |
| `TestCLOBTxSelectorValidationError` | Blocklist validation rejects tx (skip, not halt) |
| `TestCLOBTxSelectorClear` | Clear resets all counters and initialized flag |
| `TestCLOBTxSelectorBytesLimit` | tx exceeding overall byte budget halts |
| `TestCLOBTxSelectorCLOBRatioFull` | ratio=1.0: regular txs only get rollover |
| `TestCLOBTxSelectorClearAndReuse` | Clear + re-init with different maxBlockGas |
| `TestCLOBTxSelectorCLOBBytesLimit` | CLOB tx exceeding byte quota is skipped; regular tx accepted |
| `TestCLOBTxSelectorBytesRollover` | No CLOB txs: full byte budget available to regular |
| `TestCLOBTxSelectorMixedBytesAndGas` | CLOB tx rejected by bytes even when gas is available |

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
- `testSequencerAddr` — canonical sequencer address for CLOB test routing
- `testIsCLOBTx()` — classifies `testTx` as CLOB when signer matches `testSequencerAddr`
- `newTestSDKCtx()` — creates `sdk.Context` with nil multistore (safe for priority extraction)
- `newCLOBTx()` / `newRegularTx()` — create test txs with sequencer/non-sequencer signers
- `mockProposalVerifier` — bidirectional tx-to-bytes map implementing `baseapp.ProposalTxVerifier`

## Extending the Feature

### Governance-Based Sequencer Address

The current design reads the sequencer address from `app.toml` at startup.
To support on-chain governance updates, replace `NewCLOBTxDetector` with a
closure that reads from the governance module's keeper at detection time:

```go
func NewGovCLOBTxDetector(keeper ExchangeKeeper) func(sdk.Tx) bool {
    return func(tx sdk.Tx) bool {
        sequencerAddr := keeper.GetSequencerAddress()
        if len(sequencerAddr) == 0 {
            return false
        }
        for _, msg := range tx.GetMsgs() {
            ethMsg, ok := msg.(*evmtypes.MsgEthereumTx)
            if ok && bytes.Equal(ethMsg.GetFrom(), sequencerAddr) {
                return true
            }
        }
        return false
    }
}
```

### Per-Type Gas Limits

The current design has a single CLOB gas and byte quota. To support multiple
categories (e.g., CLOB + DA), extend `CLOBTxSelector` with additional
gas/byte counters and detection functions. The rollover formula generalizes to:

```
effectiveRegularGasLimit  = maxBlockGas − sum(categoryGasUsed)
effectiveRegularBytesLimit = maxTxBytes  − sum(categoryBytesUsed)
```

### Separate MaxTx per Pool

Currently both pools share the same `MaxTx`. To allow independent limits,
change `NewCLOBMempool` to accept separate `clobMaxTx` and `regularMaxTx`
parameters and build two different configs.
