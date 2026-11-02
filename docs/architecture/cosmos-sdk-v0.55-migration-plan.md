# Cronos EVM: Cosmos SDK v0.55 migration plan

Status: draft, 2026-10-29. Current main (`4b6cb201`) runs v1.8 on fork SDK `v0.54.4-0.20260924083050-23eef471a53c`, CometBFT fork `14b7b93046e3` (v0.39 + fork fixes), ethermint `cdb246e9a1d2`, ibc-go v11.1.0, Go 1.25.11.

Sources:

- SDK guide: <https://docs.cosmos.network/sdk/latest/upgrade/v0.55>
- Fork SDK port: crypto-org-chain/cosmos-sdk#1888 (fork fixes on v0.55.0; `UPGRADING.md` "crypto-org-chain Fork Changes"), stacked #1889
- Fork CometBFT port: crypto-org-chain/cometbft#71 (fork fixes on v0.40.0), stacked #72
- Pending PRs touching the same code: crypto-org-chain/cronos#2216, #2239, #2159; crypto-org-chain/ethermint#1130, #1118, #1120, #1136

## 1. Dependency chain

Every arrow is a hard block.

```
cometbft#94, #71 ─► cosmos-sdk#1888 ─► #1889 ─┬──────────────► ethermint v0.55 ─┬─► cronos
                                              └─► cronos-store ─────────────────┤
ibc-go (pinned, see below) ──────────────────────► ethermint v0.55, cronos      │
pending ethermint PRs (§5) ──────────────────────► ethermint v0.55              │
pending cronos PRs (§5) ────────────────────────────────────────────────────────┘
```

| Repo | State today | Needed |
|---|---|---|
| crypto-org-chain/cometbft | #71 open (v0.40.0 + fork #51 app-mempool fix + lp2p queue cap). Fork #50, #54, #64 already upstream. All commits in Cronos's current pin are covered. | Fix the `MedianTime` upgrade-height risk with #94, a handshake replay change (§4.3). Merge #71; skip #72 (see below). Repin SDK to the merge commit. |
| crypto-org-chain/cosmos-sdk | #1888 open on `release/v0.55.x`; #1889 stacked. | Merge both. #1889 changes the staking queue index and the upgrade handler (§4), so land it before Cronos pins. |
| crypto-org-chain/cronos-store | Pins SDK v0.54.3, CometBFT v0.39.3. `store/v2` stays `v2.0.0` in v0.55. memiavl already supports `StoreUpgrades.Deleted`. | go.mod bump (SDK, CometBFT, Go 1.26.10), plus the v1.9 roadmap work (§12): drop `iavl`, `erigon-lib` and the 2017 file-lock dependency from memiavl; fix versiondb parallelism and the per-block fsync. Run memiavl/versiondb tests; tag. Only Cronos depends on it, so this runs in parallel with the ethermint port. |
| cosmos/ibc-go | v11.2.0 still pins SDK v0.54.0. `main` (still `/v11`) moved to SDK v0.55.0 + CometBFT v0.40.0 in cosmos/ibc-go#9011 (merge commit `ae841d41a589`). `v11.2.1-rc1` (2026-10-29, commit `f5cb662b9985`) tags `main`: SDK v0.55.0, CometBFT v0.40.0, Go 1.26.10. It also carries state-machine changes after #9011 (e.g. #9033 future-revision packet timeouts, #9080 `RecoverClient`) and the Go 1.26.10 bump (#9133). | Target the v11.2.1 line: review the state-machine changes since v11.1.0 for Cronos impact and use the RC on testnet. For mainnet, use a final tag whose `go.mod` resolves to SDK v0.55.0. `release/v11.2.x` is still on SDK v0.54, so the final tag's name and branch aren't confirmed yet. If no such tag exists at the release cut, pin the reviewed RC commit `f5cb662b9985` (or `ae841d41a589`, #9011 alone). Don't track `main`. |
| crypto-org-chain/ethermint | No v0.55 branch. `develop` (default branch, which Cronos pins) is on SDK fork v0.54. | Full port (§2) after its pending PRs (§5). Tag a new minor. |

#72 content: recover the AppMempool from app errors and panics; drop failed-CheckTx txs from the seen cache after `check_tx_retry_delay`; throttled logs; a `failed_txs` metric; zero `seen_cache_size` / `check_tx_retry_delay` rejected at config validation.

**Decision: skip #72 for v1.9.** What it fixes only triggers when the app returns a Go error or panics:

- **Rejections are already handled by #71.** A tx rejected with a response code leaves the seen cache after a delay: every code on the RPC CheckTx path, and only retryable codes (≥ `CodeTypeRetry`) on the peer path, by design. Cronos reports every rejection as a code: `Manager.InsertTx` and `Manager.CheckTxHandler` always return a nil error. The only Go-error path is `App.CheckTx` with `dummyCheckTx` set, a benchmark-only mode.
- **Without #72, a panic in Cronos code degrades the node silently but doesn't crash it or affect consensus:**
  - in CheckTx, the RPC broadcast hangs until timeout;
  - in ReapTxs, the node stops gossiping its own txs until restart;
  - in a peer InsertTx, that peer is dropped.
- **Cover this in Cronos instead:** add `recover` to `NewReapTxsHandler` (return an empty reap), `Manager.InsertTx` and the CheckTx handler's non-`runTx` code (return a code, never a Go error), with tests. Alert on the node's reaped-tx metric staying flat.
- Without #72, setting `mempool.seen_cache_size` or `check_tx_retry_delay` to 0 panics at startup. Cronos's defaults are 100000 and 5s; the operator guide should say not to set either to 0.
- Revisit #72 when the fork next rebases.

## 2. Ethermint port (critical path)

Ethermint carries most of the EVM-specific breakage. Start it once its pending PRs (§5) have landed, in its own PR on ethermint `develop`.

- **x/params removal.** `x/params/types` no longer exists, and both EVM modules still read it at runtime, not only in migrations:
  - `x/evm/keeper`: `ss paramstypes.Subspace` field and constructor arg (`keeper.go:83,100`); `GetParams` falls back to `GetLegacyParams` → `k.ss.GetParamSetIfExists(V0Params)` when the params key is empty (`params.go:25-51`).
  - `x/feemarket/keeper`: same pattern (`keeper.go:41,49,60`, `params.go:32`).
  - Remove the `ss` fields, constructor args and legacy fallbacks. Decide what `GetParams` does when the key is missing (return defaults or panic) and add a test. On a live chain the key always exists once the v4 migration ran.
  - Migrations: `Migrate3to4` (evm, feemarket) needs the subspace. evm `v5`/`v6` migrations import `migrations/v0/types` and `v4/types`. Drop `migrations/v0` and `v4` only together with evm 3→4, 4→5 and 5→6, i.e. only if every live chain has evm ≥ 6 and feemarket ≥ 4 (§6). Today's versions: evm 8, feemarket 4.
- **SIGN_MODE_TEXTUAL removal.** `rpc/backend/sign_tx.go` (`eth_sign`, `eth_signTypedData`) and `x/evm/types/msg.go` pass `SignMode_SIGN_MODE_TEXTUAL` to `Keyring.SignByAddress` for raw signing. The enum value is gone. Switch to `SIGN_MODE_DIRECT` (the mode is ignored for local keys; check Ledger behavior). Remove textual wiring from `cmd/ethermintd/root.go` and `evmd/app.go`.
- **Consensus-key rotation.** v0.55 lets the consensus address → validator mapping change. Ethermint assumes it is fixed:
  - `x/evm/keeper/utils.go` `GetCoinbaseAddress` calls `GetValidatorByConsAddr(header.ProposerAddress)` and errors if not found. It runs on the EVM tx path.
  - Failing block: a rotation tx at height h emits the CometBFT validator update in EndBlock(h), so the **new** key can propose block h+2. The SDK swaps `ValidatorByConsAddr` only in EndBlock(h+2) (`ApplyConsKeyRotations`), after h+2's txs run. If the rotating validator proposes h+2, the new address isn't indexed yet and every EVM tx in that block fails.
  - `ValidatorByHistoricalConsAddr` doesn't help there: it resolves only `RotatedFrom` locks, and the new key is still `PendingTo`. Fix the coinbase path by also resolving `PendingTo` through `Keeper.GetRotationLockedConsAddr` (widen ethermint's `StakingKeeper` interface). Don't move the SDK's index swap to BeginBlock instead: distribution's `AllocateTokens` resolves `LastCommit` voters (still the old key at h+2) through `ValidatorByConsAddr` with no historical fallback and would fail BeginBlock (`x/distribution/keeper/allocation.go:59`); slashing's liveness path likely fails the same way. Report the coinbase gap upstream.
  - `x/evm/keeper/grpc_query.go` `ValidatorAccount` and `rpc/backend` (`getValidatorAccount`, blocks, chain info, fee history) look up the `miner` the same way. For old blocks, fall back to `ValidatorByHistoricalConsAddr`. That only works while the `RotatedFrom` lock exists: it is pruned at max(unbonding time, evidence age) after the rotation, 28 days on mainnet. After that, only a historical-state query resolves the old address, and with ethermint#1118 on memiavl-only nodes `miner` becomes the zero address. Document the limit, or keep a permanent old-address → validator index if `miner` must stay correct.
  - Test with the rotating validator as proposer at the apply height and the next two blocks, plus historical `miner` lookups. This is from code reading; the test confirms it. ethermint#1118 says "no consensus key rotation"; that assumption no longer holds.
- **Mempool interface.** `evmd/mempool.go` `SelectBy` callback takes `mempool.PooledTx`. Any custom `TxSelector` gains a `gasWanted uint64` argument.
- **evmd reference app.** Same app.go changes as Cronos §3 (params, textual, `key_rotation_fee_pool`, module constructors).
- **Deps.** SDK fork v0.55, CometBFT fork, ibc-go (§1), Go 1.26.10.

## 3. Cronos code changes

### 3.1 Removed surfaces

| Change | Cronos call sites |
|---|---|
| `x/params` removed | `app/app.go`: imports (L142–146), `paramsclient.ProposalHandler` (L197), `params` store key + `transient_params` tkey (L244, L261), `ParamsKeeper` + `initParamsKeeper`, `GetSubspace`, subspaces passed to ethermint keepers (`feeMarketS`/`evmS`, L786–801) and `x/cronos` (L823), `paramproposal.RouterKey` legacy gov route (L828), params in begin/end/init-genesis orders. `app/sim_test.go` (L36 import, L237 store-key compare). `client/docs/config.json` L111 (params swagger). |
| Subspace args dropped from SDK constructors | `auth`, `bank`, `gov`, `mint`, `slashing`, `distr`, `staking` `NewAppModule` (L908–925, L1097). `crisis.NewAppModule(&keeper, skipGenesisInvariants)` also lost its subspace. |
| Cronos's own legacy migration | `x/cronos`: `legacySubspace`, `Migrate1to2` (`migrations/v2`), `exported.Subspace`, `ParamSetPairs` in `types/params.go`. Drop; keep `ConsensusVersion` as is. |
| `SIGN_MODE_TEXTUAL` removed | `app/app.go` L677–692. `cmd/cronosd/cmd/root.go` L106–125: delete the whole `if !initClientCtx.Offline {…}` block and the now-unused `slices`, `txmodule`, `signing` imports. |
| `x/protocolpool` removed | Not wired in Cronos. Nothing to do. |
| `genutil.ExportGenesisFileWithTime` signature | Not used in Cronos. |

### 3.2 Changed interfaces

- **Mempool** (`types/mempool`): `Insert(ctx, tx, InsertOption{GasWanted})`, `Iterator.Tx() PooledTx`, `SelectBy(..., func(PooledTx) bool)`.
  - Prod: `app/mempool/helpers.go` `PoolSnapshot` / `UnorderedPoolSnapshot`. Cronos never calls `Mempool.Insert` itself; baseapp's `runTx` does and already passes the ante gas (`InsertOption{GasWanted: gasWanted}`).
  - Tests: fakes `fakePool`, `countingPool`, `hookPool` (`manager_test.go`), `stubMempool`/`stubIterator` (`reap_test.go`), `lockTrackingMempool` (`recheck_test.go`); `SelectBy(func(sdk.Tx))` callers in `recheck_test.go:123`; `SelectTxForProposal` callers in `app/proposal_test.go`.
- **`baseapp.TxSelector`**: `SelectTxForProposal(..., memTx sdk.Tx, txBz []byte, gasWanted uint64)`. `app/proposal.go` `ExtTxSelector` uses `feeTx.GetGas()` today. **Decision (§10.2): use the ante-reported `gasWanted` like upstream**, falling back to `GetGas()` when it is 0 (upstream's `txGasForBlockAccounting` rule). Test: a tx whose ante-reported gas differs from its declared limit is packed by the reported value.
  - Not consensus-critical for Cronos: `ProposalHandler.ProcessProposalHandler` replaces the SDK default and only runs the blocklist check, no block-gas check. It only changes block packing. Upstream's default `ProcessProposal` does enforce gas, so this would matter if Cronos ever went back to it.
- **`ProposalTxVerifier.ProcessProposalVerifyTx`** now returns `(sdk.Tx, gasWanted uint64, error)`. Cronos doesn't implement it; recompile only.
- **`app/mempool/reap.go`** reads `feeTx.GetGas()` for reap gas. Switch it to the pooled `GasWanted` with the same fallback, so reaping and proposal packing count gas the same way. `PoolSnapshot` returns `[]sdk.Tx` (`helpers.go:28`) and drops `GasWanted`, so add a variant that returns `[]mempool.PooledTx` for reap and keep the `[]sdk.Tx` one for the Manager's other callers.
- The `ExtTxSelector` base-fee gate (fee cap = fee / `GetGas()`, `proposal.go:96-99`) keeps using the declared gas limit; only block-gas accounting switches.
- **Staking hooks**: `StakingHooks` adds `AfterValidatorConsKeyUpdated`. Cronos only chains SDK `distr` + `slashing` hooks, so no change unless a custom hook is added.

### 3.3 New required wiring

- `maccPerms[stakingtypes.KeyRotationFeePoolName] = {authtypes.Burner}`. The staking keeper panics at construction without it.
- `BlockedAddrs()` builds from `maccPerms`, so the new account is blocked for bank sends automatically. EVM value transfers don't check `BlockedAddrs()` (ethermint's `statedb` transfer goes through `bankKeeper.SendCoins` directly), so plain EVM transfers to the pool still work after the upgrade, as they do for every other module account. That's harmless here: rotation only sends and burns the fee itself, so extra funds just stay stuck in the pool. §4.1 covers the pre-upgrade case.

### 3.4 Stricter validation to cover in tests

No code change expected, but each can reject txs that v0.54 accepted:

- `SetPubKeyDecorator` rejects a tx whose `SignerInfo` count differs from its signer count (`ErrTxDecode`, cosmos/cosmos-sdk#26573). Covers Cosmos txs and EIP-712 txs through ethermint's Cosmos ante chain.
- Multisig: bit-array size must match the key set (#26515), and nested multisig signatures are capped at depth 2 and 32 total (`flattenSignatures`, `ErrTooManySignatures`). Check whether any Cronos account uses nested multisig.
- `secp256k1.PubKey` unmarshal requires tag byte `0x02`/`0x03` (#26529). Ethermint's `ethsecp256k1` is a separate type and unaffected.
- `Any` recursion cap in unknown-field checks drops from 10,000 to 64 (#26587). Check nested `authz.MsgExec` payloads.
- Fork change: an error from the `AfterUnbondingInitiated` staking hook now fails `MsgUndelegate`/`MsgBeginRedelegate`. Cronos only chains the SDK `distr` and `slashing` hooks (`app.go:757-760`), which don't error there; no change expected.
- Fork change already on v0.54: CheckTx inserts into the mempool before writing the ante state (`baseapp.go` in the fork, `execModeCheck`). Cronos runs this today; no change.

### 3.5 Other Cronos code in v1.9

- **Mempool panic recovery** (instead of cometbft#72, §1): `recover` in `NewReapTxsHandler` (return an empty reap), `Manager.InsertTx` and the CheckTx handler's code outside `runTx`. Each returns a response code, never a Go error.
- **`x/e2ee` hybrid keys** (§12.3, decision 9):
  - Proto and state: a second, optional hybrid key per address under a new store prefix. Register it with a new message or a key-type field on `MsgRegisterEncryptionKey`.
  - Queries return both keys, plus a way to check that every bonded validator has a hybrid key.
  - Genesis import/export carries both keys.
  - Bump `x/e2ee` `ConsensusVersion` 1 → 2 with a no-op migration (both chains are at 1, §6).
  - `ValidateRecipientKey` accepts hybrid keys for the new field only.
  - The CLI (`generate`, `pubkey`, `encrypt`, `encrypt-to-validators`, `decrypt`) handles hybrid keys.
  - The node and `ProposalHandler` load both identities (`app.go:415-433`, `proposal.go:225`).

### 3.6 Optional / keep-as-is

- **Block-STM**: Cronos already uses `baseapp/blockexec` + `SetDisableBlockGasMeter(true)` (backported in the v0.54 fork). Recompile only.
- **`contrib/x/crisis`** still exists in v0.55. Decided (§10.4): keep it for v1.9; removing it would add `crisis` to the store deletions.
- **ML-DSA-65 / secp256k1eth consensus keys**: ship the code, keep `pub_key_types = ["ed25519"]` at the upgrade. ML-DSA-65 migration is a separate phase (§11).
- **Behavior changes with no code change**: begin/end-block reward withdrawals to blocked addresses fall back to the owner, then the community pool (#26406); `x/authz` prunes at most 200 expired grants per block (#26588).

## 4. Upgrade handler and store migrations

Next plan name: `v1.9`, per the v1.9 roadmap proposal (§12). `docs/migrations/v1.8-to-v1.9.md` already exists on main; #2216 extends it.

```go
upgradeInfo, err := app.UpgradeKeeper.ReadUpgradeInfoFromDisk()
if err != nil {
    panic(err)
}
if upgradeInfo.Name == planName && !app.UpgradeKeeper.IsSkipHeight(upgradeInfo.Height) {
    storeUpgrades := storetypes.StoreUpgrades{
        Deleted: []string{"params"}, // add "crisis" only if crisis is removed
    }
    app.SetStoreLoader(MaxVersionUpgradeStoreLoader(maxVersion, upgradeInfo.Height, &storeUpgrades))
    return true
}
return false
```

- Return `true` only when the upgrade loader was installed; otherwise the caller sets `MaxVersionStoreLoader` (app.go L1082–1085).
- `MaxVersionUpgradeStoreLoader` exists in `app/storeloader.go` but has never been used or tested. With versiondb on, it calls `LoadLatestVersionAndUpgrade`, which ignores the versiondb version cap at the upgrade height. Add a unit test for both cases before relying on it.

Handler steps, in order:

1. **Pre-create `key_rotation_fee_pool`** before `RunMigrations` (§4.1).
2. `RunMigrations`: `x/staking` 5 → 6 (adds `key_rotation_fee`, default `1000000` of bond denom `stake`; `Params.Validate` requires `fee.denom == bond_denom`), `x/auth` 6 → 7 (adds `SigVerifyCostMlDsa65`), `evm` 8 → 9 (ethermint#1120, see step 6). v0.55 has no migrations left for gov, bank, slashing, distribution or crisis, so those modules must already be at their v0.54 versions (§6).
3. **Staking queue index.** With #1889 merged, `Migrate5to6` recomputes the earliest-time gate keys (`0x44`–`0x46`) and `PopulateQueuePendingSlots` is a deprecated alias: drop the call. Without #1889, keep the call (v1.8 already ran it; rerun is safe).
4. **Set staking params**, after `RunMigrations` (which writes the default fee):
   - `key_rotation_fee = 1stake`, the smallest value `Params.Validate` accepts (must be positive). The default `1000000stake` would block most operators: on 2026-10-29, 25 of 29 bonded operators held 0 liquid `stake` (the fee is paid from the operator account, not from bonded tokens). Rotation spam is already limited to one per validator per 28 days.
     - Operators still need ≥ 1 liquid `stake`. Before any rotation (ML-DSA phase or an emergency key replacement), the team that manages the permissioned `stake` supply sends each operator a small amount from its liquid `stake` balance (≥ 1 base unit per planned rotation). Add this to the operator runbook.
   - `historical_entries = 0`, **only if EIP-2935 already covers the last 256 blocks**:
     - `GetAccount(HistoryStorageAddress)` is a contract;
     - the effective window (`history_serve_window`, where 0 means 8191) is ≥ `header_hash_num` (256);
     - `GetHeaderHash` is non-zero for U-1 … U-256, which also catches a contract deployed less than 256 blocks ago.

     Otherwise leave the param unchanged and log why. Without the contract, `BLOCKHASH` for recent blocks would fall back to the now-empty `HistoricalInfo` and return the zero hash. Mainnet and testnet have the contract (§6); the guard protects test chains, devnets and testground, which run the same handler. Each `HistoricalInfo` entry holds the full block header and validator set (~25 KB as JSON) and is written every block into memiavl and versiondb. Only ethermint's `BLOCKHASH` fallback reads it (§11.3).
     - The handler must also delete the existing 10,000 entries. With the value at 0, `TrackHistoricalInfo` starts pruning at the current height, finds nothing and stops, so old entries would stay forever. Loop from the upgrade height − 1 downward and stop at the first missing entry.
     - Test that `BLOCKHASH` for the last 256 blocks still returns the right hashes after the upgrade.
5. Drop the v1.8-only steps (IBC prune, `CroBridgeContractAddresses`) once v1.8 is the floor.
6. **EVM storage compaction (ethermint#1120)**, run as part of step 2 (no extra handler code). The `evm` 8 → 9 migration (mainnet and testnet are at 8, §6) turns on `compact_storage` and starts the zero-slot sweep. What follows:
   - **Sweep cost.** The sweep deletes zeroed slots at 10k slots per block for ~12.5k blocks (~1.7 h at 0.48 s). One batch costs 25–40 ms on a 100k-slot tree and more on mainnet's ~125M slots; measure it on a mainnet snapshot, because at 0.48 s blocks that is ≥ 5–8% of the block.
   - **Storage.** memiavl state shrinks ~29% after the sweep. Existing versiondb nodes grow ~4% (+143 MB of delete markers).
   - **Proofs.** `eth_getProof` storage proofs cover the trimmed stored bytes, and a deleted zero slot returns a non-existence proof. Tell bridges and anyone verifying EVM storage proofs.
   - **Gas.** EVM calls made from Cosmos messages (e.g. `CallEVM`) use less KV gas after the switch. Ethereum txs are unchanged.
   - **Export.** The sweep cursor isn't exported. A chain restarted from a pre-v9 export sets `compact_storage: true` in genesis.

### 4.1 Security: `key_rotation_fee_pool` pre-funding grief

The pool's address is deterministic and public. Anyone can send CRO (bank or plain EVM transfer) to it **before** the upgrade height, which creates an `EthAccount` there. After the upgrade, `MsgRotateConsPubKey` → `SendCoinsFromAccountToModule` → `GetModuleAccountAndPermissions` panics with `"account is not a module account"`, so every rotation fails (the panic is recovered per tx). It is a cheap, permanent DoS on key rotation.

Fix in the handler: if an account exists at the pool address and is not a `ModuleAccountI`, replace it with a module account that keeps its account number and balance. Then call `GetModuleAccount` once so the account exists before any tx. Add a test that pre-funds the address on the pre-upgrade binary.

Live check 2026-10-29: the address (`0x03d9eba4c892f3066b2d1e30853e4afff0bdf959`, `crc1q0v7hfxgjtesv6edrccg20j2llctm72et9asqw` / `tcrc1q0v7hfxgjtesv6edrccg20j2llctm72e9s55ql`) has no account and no balance on mainnet or testnet. The fix is still needed, because anyone can fund it any time before the upgrade height. Re-check right before the upgrade.

### 4.2 Historical data that x/params removal breaks

- **Gov proposals.** All `cosmos.params.v1beta1` protos are gone (no Go types, no `api/` types). Both chains hold historical `ParameterChangeProposal`s inside `MsgExecLegacyContent`: mainnet proposals 3, 4 and 8; 13 proposals on testnet (7 passed, 6 rejected; rejected ones are stored too) (§6). After the upgrade, without a fix:
  - gov `ExportGenesis` panics while unpacking the unknown `Any`, so `cronosd export` fails;
  - one undecodable proposal fails the whole `q gov proposals` list, not just that entry;
  - tx queries and indexers can't decode the `MsgSubmitProposal`/`MsgExecLegacyContent` that carried them.
- **Mitigation.** Vendor the params proto types under `app/legacy/params` and register them decode-only as `govv1beta1.Content` in `RegisterLegacyInterfaces`, the same pattern as the removed `icaauth` and ICA types. Keep the legacy gov route removed (app.go L828) so new `ParameterChangeProposal` submissions are rejected; test both. Verify against a mainnet export.
- **Historical queries.** `x/params` gRPC routes are gone; queries at pre-upgrade heights need the old binary. Document for archive operators.
- **In-flight proposals.** Any open legacy params proposal at the upgrade height won't execute. None are open on either chain today; re-check before scheduling.

### 4.3 CometBFT `MedianTime` change can halt the chain at the upgrade height

In plain terms: each block's timestamp isn't chosen freely. It's the stake-weighted median of the timestamps on the previous block's precommit votes, and every node recomputes it to check the block. CometBFT v0.40 changes which votes count: it drops "nil" votes (validators who voted for no block). If the old and new binaries compute different times for the upgrade block, the new binary rejects a block the old binary already committed, and the chain can't move forward.

The details: CometBFT v0.40 (cometbft#5901) makes `MedianTime` skip `Nil` precommits; v0.39.3 skipped only `Absent`. `validateBlock` requires `block.Time == MedianTime(block.LastCommit)`, and the change has no height gate.

- The upgrade-height block H is proposed and committed by v0.39 binaries. CometBFT saves block H and its seen commit before `FinalizeBlock`, then the SDK halts in `PreBlock`.
- Without #94, on restart the v0.40 binary replays H through `Handshaker.replayBlock` → `ApplyBlock` → `validateBlock` (normal consensus uses `ApplyVerifiedBlock`, but the handshake doesn't), using the new rule on H's `LastCommit` (the commit for H-1).
- If that commit contains a `Nil` precommit that moves the weighted median, validation fails with `invalid block time` on every node, and the chain halts.
- **Measured on mainnet** (recomputing both rules from REST block data):

  | Sample | Blocks | Commits with a `Nil` precommit | v0.39 rule matches header | v0.40 would reject |
  |---|---|---|---|---|
  | 2026-10-29, ending 102,782,600 | 300 | 6 | 8 of 8 checked | 2 (≈0.7%) |
  | 2026-11-01, ending 103,309,300 | 2,000 | 49 | 49 | 32 (1.6%) |
  | Oct 2026, ending 98,000,000 | 1,000 | 6 | 6 | 2 (0.2%) |

  So an unpatched upgrade would halt in roughly 0.2–1.6% of cases. The rate depends on how often validators precommit nil, so it is not constant.
- The old binary can't recover it either, because it halts again at H. Only a patched v0.40 binary gets the chain going.
- The same rule makes v0.40 unable to blocksync any pre-upgrade block whose commit has a `Nil` precommit that moves the median. That doesn't change the sync procedure: nodes syncing from genesis or a pre-upgrade snapshot already run the old binary up to H through cosmovisor, because the new app can't execute old blocks.

**Decision (§10.7): keep #5901 unconditional and skip re-validation of the replayed tip block** (crypto-org-chain/cometbft#94, suggested in review):

- **The change.** `Handshaker.replayBlock` applies the stored tip with `ApplyVerifiedBlock` instead of `ApplyBlock`. Every stored block was validated before it was saved (consensus `finalizeCommit`, blocksync), and the handshake already replays earlier blocks unvalidated through `ExecCommitBlock`. Block H was validated by the v0.39 binary under its own rule, so the new binary applies it as is.
- **After H.** Every block from H + 1 on is proposed and checked by v0.40 binaries under the same rule, so nothing else needs a gate.
- **No Cronos code.** No consensus param, no proto field, no upgrade handler step, nothing operators configure. The fork's consensus rule matches upstream.
- **Known limit.** A v1.9 node whose state is exactly H-1 without block H stored (a state-sync snapshot at H-1, or a binary swapped before H arrived) fetches H through blocksync, which still validates it and can reject it. That node stalls; it recovers by running v1.8 through H. The network isn't affected, because the validators that committed H have it stored. Put this in the operator guide: cross the upgrade height on the v1.8 binary.
- **Tradeoff.** The handshake no longer cross-checks the tip's header against the local state. A mismatched block store and state DB now fails one block later, with a less direct error.
- **Earlier designs, dropped.** A per-chain-id height hard-coded in the binary, then a `MedianTime.EnableHeight` consensus param set by the upgrade handler. Both worked but added fork-only consensus logic and halt risks of their own (a wrong value in the handler halted the chain). Reverting #5901 permanently was also considered and not chosen.

Raise it upstream too, since every v0.39 → v0.40 chain has the same exposure. Test in the CometBFT fork: build block H whose `LastCommit` has a `Nil` precommit that moves the median and whose time follows the v0.39 rule. Run the handshake with app = state = H-1 (real app) and with app = H, state = H-1 (mock app). Assert both pass and keep H's time, and that `ValidateBlock` still rejects H outside the handshake. Cronos has no integration test for it; the fork's unit test covers it.

## 5. Pending PRs: land before the migration branch

All three Cronos PRs touch `app/mempool`, which is where most v0.55 mempool interface churn lands. Ethermint PRs touch code the port rewrites.

| PR | Overlap with v0.55 work | Recommendation |
|---|---|---|
| cronos#2216 commit priority + RPC in-flight cap (ready) | `app/app.go`, `app/mempool/manager.go`, `cmd/cronosd/config`, `test_upgrade.py`, extends `docs/migrations/v1.8-to-v1.9.md` | Merge first. Its upgrade test and migration doc are the base the v0.55 upgrade extends. |
| cronos#2239 pool tx lookup for tx replacement (draft) + ethermint#1130 | `app/app.go`, `app/mempool/manager.go`, `app/proposal.go` (`ExtTxSelector`), `go.mod`, `test_upgrade.py` | Land together before the ethermint port starts, or both sides get redone. |
| cronos#2159 recheck per signer group (draft) | `app/mempool/manager.go`, `exec.go`, `scheduler.go`, recheck tests | Don't block the branch cut: rebase onto `feat/sdk-v0.55` and switch its `SelectBy` usage to `PooledTx`. |
| ethermint#1118 skip historical state in block RPCs | `rpc/backend` validator lookups (also touched by §2 key rotation) | Land before the port; then apply the §2 rotation fix on top. |
| ethermint#1120 compact EVM storage + sweep zeroed slots | Adds evm v8 → v9 migration, the `compact_storage` param, and a BeginBlock sweep (10k slots/block for ~12.5k blocks) | **In v1.9.** Same evm package as #1136; they share only `keeper/statedb_test.go`, but #1120 changes the `SetState` encoding that #1136's statedb rework drives. Land #1136 first, rebase #1120 on it and re-run both statedb test suites, all before the SDK port. Upgrade and test impact are in §4 and §8. |
| ethermint#1136 go-ethereum v1.17.10 (draft) | `go.mod`, `x/evm/types/msg.go` (§2 textual), `x/evm/keeper/utils.go` and `grpc_query.go` (§2 rotation fix), other `x/evm/keeper` and statedb files | In v1.9 scope per the roadmap (§12). Land it on ethermint `develop` before the SDK port starts, and rebase the port on it, so EVM and SDK regressions can be bisected separately. Both ship in the one v1.9 upgrade. |

Cut `feat/sdk-v0.55` from main after cronos#2216 and cronos#2239 land.

## 6. Live-chain checks

Probed 2026-10-29 through the public REST endpoints (`rest.cronos.org`, `rest-t3.cronos.org`) and EVM JSON-RPC. CometBFT RPC and `node_info` sit behind Cloudflare/403 and weren't used. Mainnet height ~102.78M; both chains run v1.8.

| Check | Required | Mainnet | Testnet |
|---|---|---|---|
| Module versions | auth 6, staking 5, gov 5, bank 4, slashing 4, distribution 3, crisis 2, mint ≥ 2; evm ≥ 6, feemarket ≥ 4, cronos ≥ 2, e2ee 1 | auth 6, staking 5, gov 5, bank 4, slashing 4, distribution 3, crisis 2, mint 3; evm 8, feemarket 4, cronos 2, e2ee 1 ✅ | identical ✅ |
| Historical `ParameterChangeProposal`s (§4.2) | none, or vendor the types | 3 (ids 3, 4, 8) ❗ | 13 ❗ |
| Open gov proposals | none at upgrade | none ✅ | none ✅ |
| `key_rotation_fee_pool` account (§4.1) | absent | absent, zero balance ✅ | absent, zero balance ✅ |
| `block.max_bytes` / `evidence.max_bytes` (§7) | fits v0.40 commit reservation | 1,048,576 / 150,000 ✅ (tx space −93 KB: ~1,044 → ~950 KB typical, ~894 → ~800 KB with max evidence) | same ✅ |
| `validator.pub_key_types` | `["ed25519"]` | `["ed25519"]`; 29 bonded, all ed25519 | `["ed25519"]`; 29 bonded |
| Staking params | — | unbonding 28 d, `max_validators` 30, `historical_entries` 10000, bond denom `stake` | unbonding 28 d, `max_validators` 35, same otherwise |
| Evidence max age | — | 210,000 blocks / 1 day | 403,200 blocks / 28 days |
| EIP-2935 history contract (§11.3) | deployed, window ≥ 256 | deployed, `prague_time` 0, window 8191 (default) ✅ | deployed, window 8191 ✅ |
| Block interval | — | ~0.48 s (285 blocks in 137.6 s), ~179k blocks/day | — |
| `Nil` precommits that move the median (§4.3) | 0 | 0.2–1.6% of blocks across 3 samples ❗ | not sampled |
| Live IBC counterparties (§11.4) | — | 43 clients: 5 active (Cronos POS, Cosmos Hub, Akash, Celestia, Juno), 38 expired (28 of them on 8 chains with no live client, incl. Osmosis) | Croeseid (Cronos POS testnet), Celestia `mocha-4`, Cosmos Hub testnets, others |

Still open (not visible from the chain):

- how validators sign (file key, tmkms, HSM);
- the code-level check that nothing reads the `params` store at runtime before it's deleted (§2/§3.1 removals cover the known readers).

The mainnet module-version map also lists removed modules (`capability`, `feeibc`, `icaauth`, `interchainaccounts`, `vesting`, and `params` after the upgrade). `RunMigrations` only walks modules in the manager, so these leftovers are harmless.

## 7. Config and operator-facing changes

- **gRPC `max-send-msg-size` default drops from ~2 GB to 10 MB** (fork #1830). Ethermint's JSON-RPC already caps in-process gRPC replies at `max-recv-msg-size` (10 MB default), so JSON-RPC only changes for operators who raised the recv limit; direct gRPC clients with large queries are affected. Cronos's `app.toml` template comes from `servercfg.AppConfig`, so new files get 10 MB. To keep the old value, override it in `initAppConfig`; either way, call it out in the operator guide.
- **CometBFT v0.40** (ABCI is unchanged from v0.39.3; `InsertTx`/`ReapTxs` already exist):
  - `MedianTime` change: see §4.3.
  - **Block size accounting.** `MaxSignatureSize` now includes ML-DSA-65 (3309 B) unconditionally, so `MaxCommitSigBytes` grows from ~140 B to ~3.36 KB per validator. `MaxDataBytes` shrinks by that amount × validator count (`MaxDataBytes` counts the active set) and panics with "Negative MaxDataBytes" if `block.max_bytes` is too small.
    - Mainnet (`max_bytes` 1,048,576, 29 bonded): tx space drops by ~93 KB per block. `MaxDataBytes` subtracts the evidence actually pending, not `evidence.max_bytes`:
      - typical (no evidence): ~1,044 KB → ~950 KB (−9%); ~947 KB at the 30-validator cap;
      - worst case (150 KB of evidence): ~894 KB → ~800 KB; ~797 KB at the cap.
      - No panic risk.
    - Testground: check its `block.max_bytes`, and expect lower packing capacity in the 16-node benchmarks.
  - With `mempool.type = "app"` (fork): an app client error on `CheckTx` now returns code `1` (`CodeTypeInternal`) instead of hanging until timeout. Check RPC clients and integration tests that expect a timeout.
  - Generated `config.toml`: `mempool.max_tx_bytes` default 1 MB → 4 MB (#5989). Cronos reads this key for the decode cache cap (`FlagMempoolMaxTxBytes`), so new nodes cache larger txs. Pin it in Cronos's config template if 1 MB should stay.
  - CometBFT JSON-RPC rejects non-integer or out-of-range request IDs (#5861). Check pystarport/testground clients that hit port 26657.
  - `DefaultBlockParams().MaxBytes` grows (~53 MiB); new chains only, mainnet keeps on-chain params.
- **libp2p** (`p2p.libp2p.enabled = true` only): per-reactor `max_queue_size` (default `200000`, `[p2p.libp2p.scaler]`). Relevant to the 16-node libp2p benchmarks.
- **Key rotation** is on for every v0.55 chain once wired. The upgrade sets `key_rotation_fee = 1stake` (§4 step 4); the operator runbook covers getting liquid `stake` and the rotation steps (§11.2). Indexers keyed by consensus address must handle rotation events (`rotate_cons_pubkey`, `apply_cons_pubkey_rotation`).
- **`x/e2ee` hybrid keys** (runbook, §12.3):
  - validators generate a hybrid key, register it next to their X25519 key, and load both identities;
  - the blocklist admin switches the blocklist to hybrid once every bonded validator has a hybrid key;
  - validators then drop the X25519 identity.
- **Block selection** now counts the ante-reported `gasWanted` (§3.2), so block contents can differ for txs whose ante gas differs from their declared limit.
- **`historical_entries = 0`** (§4 step 4): `q staking historical-info` returns "not found" for every height after the upgrade.
- **Mempool config:** don't set `mempool.seen_cache_size` or `check_tx_retry_delay` to 0 (crashes at startup without cometbft#72, §1). Alert when the node's `reaped_txs` metric stays flat while the pool is non-empty.
- **Go 1.26.10** (fork; upstream v0.55.0 needs 1.26.5):
  - `go.mod`, `gomod2nix.toml`;
  - nix: `nix/default.nix`, `flake.nix`, `nix/build_overlay.nix` (Windows `go_1_25` override), `nix/golangci-lint.nix`;
  - `testground/Dockerfile` (`golang:1.25.11-bookworm`);
  - workflows: `test.yml` (`nix#go_1_25`), `codeql-analysis.yml`, `lint.yml`, `sims.yml`, `dependabot-ethermint.yml`, `dependabot-update-all.yml`, `dependencies-review.yml`.

## 8. Test plan

- Unit: `go test ./...` with `-tags objstore`, `-race` on `app/mempool`. Table-driven tests for:
  - legacy params types decode, and new legacy params proposals are rejected;
  - `key_rotation_fee_pool` pre-funded account conversion;
  - `MaxVersionUpgradeStoreLoader` deleting `params` on memiavl and IAVL, with and without versiondb;
  - `ExtTxSelector` and reap gas source: a tx whose ante-reported gas differs from its declared limit is packed and reaped by the reported value, with the `GetGas()` fallback when it is 0; the base-fee gate still uses the declared limit;
  - ethermint `GetParams` with the params key missing, and coinbase lookup across a key rotation;
  - a panic injected into the ReapTxs handler, `Manager.InsertTx` and the CheckTx handler returns an empty reap or an error code, never a Go error (stands in for cometbft#72);
  - ML-DSA account tx signing and gas (§11.0);
  - upgrade handler step 4:
    - `key_rotation_fee = 1stake` after `RunMigrations`;
    - with full EIP-2935 coverage: `historical_entries = 0`, every entry from U-1 downward deleted, and from `BeginBlock(U)` on none are written;
    - without the contract, with a window < 256, or with fewer than 256 filled ring slots: the param is unchanged;
  - `x/e2ee`:
    - register and query both keys;
    - genesis round-trip;
    - the v1 → v2 migration;
    - `ValidateRecipientKey` per field;
    - a proposal decrypts with either identity loaded.
- Integration (pystarport):
  - full suite;
  - `test_upgrade.py` v1.8 → v1.9 with versiondb enabled (the §4.3 case is a CometBFT fork unit test). Before the upgrade: a pre-funded pool address and a historical params proposal. After the upgrade, assert:
    - the staking params;
    - `BLOCKHASH` for the last 256 blocks;
    - `q staking historical-info` returns not found;
    - the evm/e2ee module versions;
  - `test_app_mempool.py` for the CheckTx error code change;
  - a key rotation with EVM txs around the apply height;
  - `x/e2ee` hybrid rollout: validators register hybrid keys while the blocklist stays X25519, then the blocklist switches to hybrid with `encrypt-to-validators`.
- Upgrade package (`integration_tests/configs/upgrade-test-package.nix`): add a `released1_8` entry and rename `current` from `"v1.8"` to `"v1.9"`.
- Mainnet-state dry run: in-place upgrade on a mainnet snapshot (memiavl + versiondb), then `cronosd export` and historical gov/tx queries. Record the upgrade handler's duration (about 10,000 `HistoricalInfo` reads and deletes in one PreBlock, plus migrations). Let the ethermint#1120 sweep run to the end and record its per-block BeginBlock time, total duration, and memiavl/versiondb size change.
- Performance: testground benchmark vs v1.8 baseline (block time, admission, max block). Watch CometBFT v0.40 block size accounting and gas-selection changes.
- Sims: `TestAppImportExport`, `TestAppStateDeterminism` after `x/params` removal.

## 9. Delivery sequence

1. Merge cometbft#94 (handshake tip replay, §4.3) and cometbft#71 (without #72) in the CometBFT fork → repin SDK.
2. Merge cosmos-sdk#1888, #1889 → tag fork v0.55.x.
3. §6 checks ran 2026-10-29. Re-run the pool-address and open-proposal checks right before submitting the upgrade proposal.
4. In parallel:
   - cronos-store bump + tag; ibc-go pin (§1);
   - land cronos#2216, cronos#2239 + ethermint#1130, ethermint#1118, ethermint#1136 (go-ethereum v1.17, §12), then ethermint#1120 rebased on it; cronos#2159 rebases onto the migration branch later (§5).
5. Ethermint v0.55 port (§2) → tag.
6. Cronos PRs on `feat/sdk-v0.55`:
   - deps + §3.1–§3.4 + legacy types (§4.2);
   - the upgrade handler (§4);
   - mempool panic recovery (§3.5), which can land on main first since it doesn't depend on v0.55;
   - `x/e2ee` hybrid keys (§3.5, §12.3).
7. §8 tests, testnet upgrade, operator guide (`docs/migrations/`), mainnet proposal. One release serves both networks:
   - point Cronos's own `go.mod` `replace github.com/cometbft/cometbft` at the fork commit with #94 (Go ignores the SDK's `replace`), and regenerate `gomod2nix.toml`;
   - the operator guide says to cross the upgrade height on the v1.8 binary (§4.3 known limit).
8. After the upgrade is stable: ML-DSA-65 phase (§11).

## 10. Decisions

All items below are decided (2026-10-29); struck-through labels mark the original question.

1. ~~Target release~~: settled as v1.9 by the roadmap proposal (§12).
2. ~~Block-packing gas~~: decided, ante-reported `gasWanted` with `GetGas()` fallback, in `ExtTxSelector` and reap (§3.2).
3. ~~`key_rotation_fee`~~: decided, `1stake`, set in the upgrade handler; operators get a small liquid `stake` balance before any rotation (§4 step 4).
4. ~~`x/crisis`~~: decided, keep for v1.9.
5. ~~cometbft#72~~: decided, skip for v1.9; cover panics in Cronos code instead (§1).
6. ~~ethermint#1120~~: decided, ships in v1.9 (§4 step 6).
7. ~~CometBFT `MedianTime` change~~: decided, keep cometbft#5901 unconditional and have the handshake apply the stored upgrade block without re-validating it (§4.3). Revised 2026-11-02 from a consensus param set by the upgrade handler, which had replaced a hard-coded per-chain-id height on 2026-10-30.
8. ~~ML-DSA-65~~: decided, support it. v1.9 ships the capability and the Cronos pieces it needs (§11.0). Turning validator keys on is a governance step after v1.9: testnet first, mainnet once the §11.4 blockers clear.
9. ~~`x/e2ee` hybrid keys~~: decided, in v1.9, storing a hybrid key next to the X25519 key per address (§12.3).
10. ~~`historical_entries`~~: decided, set to 0 and prune existing entries in the upgrade handler when EIP-2935 covers the last 256 blocks (§4 step 4).
11. ~~ibc-go version~~: decided, the v11.2.1 line after reviewing its state-machine changes: RC `f5cb662b9985` on testnet; on mainnet, a final tag that resolves to SDK v0.55.0, else the reviewed RC commit (§1).
12. ~~cronos#2159~~: decided, don't block the branch cut; rebase it onto `feat/sdk-v0.55` (§5).
13. ~~Static-context precompile `readonly`~~: decided, private report first. Later finding: Cronos registers no stateful precompiles (§12.1), so Cronos has no exposure and nothing to backport to v1.8. The private report goes to the go-ethereum fork and ethermint maintainers, so other chains that register precompiles can assess it.

## 11. Phase 2: validator migration to ML-DSA-65 consensus keys

Goal: post-quantum consensus security, i.e. every validator signing with `ml_dsa_65` (NIST FIPS 204). This is **not** part of the v0.55 upgrade handler. v0.55 + CometBFT v0.40 only make it possible; turning it on is a governance and operator rollout after the upgrade is stable.

### 11.0 What v1.9 ships for ML-DSA

v0.55 and CometBFT v0.40 bring the key types. Cronos needs these on top so ML-DSA can be turned on later without another binary:

- **Validator keys:** the ethermint coinbase/`miner` fix for rotation (§2), the `key_rotation_fee_pool` handler fix (§4.1), and `cronosd init --consensus-key-algo ml_dsa_65` working in Cronos's CLI. Test: a rotation to ML-DSA on a local net, with EVM txs around the apply height.
- **Account keys (Cosmos txs only):** add `ml_dsa_65` to `cronosd`'s keyring algorithms (`root.go:79` allows only `eth_secp256k1`/`secp256k1` today). Test: sign and broadcast a Cosmos tx with an ML-DSA account; check gas uses `SigVerifyCostMlDsa65`.
- **Inbound IBC:** works with CometBFT v0.40. Cronos's light clients can verify counterparties that adopt ML-DSA.
- **Chain params at the upgrade:** `pub_key_types` stays `["ed25519"]`. Turning on `ml_dsa_65` is a later gov proposal (§11.2), on testnet first.

### 11.1 Scope: what it does and doesn't protect

- Protects: consensus votes, commits, and therefore light-client/IBC verification of Cronos headers.
- Does not protect:
  - user accounts: EVM accounts stay `eth_secp256k1`. SDK ML-DSA account keys (`--algo ml_dsa_65`, `SigVerifyCostMlDsa65`) work only for Cosmos txs and can't sign EVM txs;
  - p2p node keys (stay ed25519);
  - validator operator keys, which still sign `MsgRotateConsPubKey` and staking txs.

### 11.2 Mechanics

1. **Enable the key type** with a gov proposal to `x/consensus` `MsgUpdateParams` (`tx consensus update-params-proposal`). `MsgUpdateParams` requires the block, evidence and validator params (abci and authority are optional), and any omitted inner field becomes zero. `block.max_gas = 0` and `evidence.max_bytes = 0` still pass validation, so a careless proposal silently changes them. Fetch the current `cronosd q consensus params`, change only `validator.pub_key_types` to `["ed25519", "ml_dsa_65"]`, and submit every other value unchanged. This must happen before any rotation. Never remove `ed25519` while any validator uses it: the next voting-power update for that validator fails validation and halts the chain.
2. **Each validator rotates** with `tx staking rotate-cons-pub-key <new ml_dsa_65 pubkey>`:
   - Generate the key offline (`cronosd init <moniker> --chain-id <cronos chain-id> --consensus-key-algo ml_dsa_65 --home <scratch dir>`). It also writes a throwaway genesis and node key there; keep only `priv_validator_key.json`.
   - Timing: if the tx executes at height h, the validator still signs with the old key at h and h+1, and the new key must sign from h+2. Swap after block h+1 commits and before h+2: replace `priv_validator_key.json` and restart the validator node (or start an already-synced standby node holding only the new key and stop the old one). Missing the window costs missed blocks, not a fault.
   - At the swap, remove the old key file from that host. Never let the old key run on two nodes, and never copy it to a second node. Two signers on one key is a double sign and tombstones the validator.
   - One rotation per validator per unbonding period (28 days on Cronos), so a bad rotation can't be fixed for 28 days.
   - The operator account pays `key_rotation_fee` (burned) from its **unbonded** `stake` balance, plus gas in CRO. Gas covers tx size (~10 gas/byte, ~22k gas for the ~2 KB pubkey) and several ~2 KB state writes (30 gas/byte), and fees go through ethermint's dynamic fee checker. Estimate on testnet; treat `--gas-adjustment 1.5` as a floor.
   - By default the key is a random raw keypair with no mnemonic, so back up the key file itself. `init --recover` derives it deterministically from a mnemonic instead, which gives a second way to restore it.
3. **Done** when every validator reports `cometbft/PubKeyMlDsa65` in `/validators`. At only 2/3 there is no fault margin: an attacker who can forge the remaining ed25519 keys (just under 1/3) breaks safety with one more Byzantine validator, and light clients trust 1/3.
4. **Residual risk after rotation:** evidence against a rotated-away ed25519 address stays admissible until both `evidence.max_age_num_blocks` and `max_age_duration` have passed (mainnet: 210,000 blocks ≈ 28 h at 0.48 s, and 1 day; testnet: 28 days). Until then, a quantum attacker could forge double-sign evidence with the old key and slash or tombstone the validator.

### 11.3 Cost for Cronos

Sizes: pubkey 1,952 B vs 32 B; signature 3,309 B vs 64 B. Each commit vote also carries ~40 B of address, timestamp and flag. Cronos numbers use the measured mainnet inputs (§6): 29 bonded validators, ~0.48 s blocks, ~179k blocks/day:

| | ed25519 (today) | ML-DSA-65 (all 29 rotated) |
|---|---|---|
| One commit (29 votes) | ~3.0 KB | ~97 KB |
| Blockstore commit data per height (stored 3×: block `LastCommit`, block-commit key, seen commit) | ~9 KB | ~291 KB |
| Blockstore commit data per day / year (before pruning) | ~1.6 GB / ~0.6 TB | ~52 GB / ~19 TB |
| App state: staking `HistoricalInfo` pubkey bytes per block (29 validators), if `historical_entries` > 0 | ~0.9 KB | ~57 KB (~10 GB/day into memiavl WAL and versiondb); 0 after v1.9 sets `historical_entries = 0` |
| Bandwidth per height per peer link (58 votes + block `LastCommit`, both with ~40 B per-vote overhead) | ~9 KB | ~291 KB (~0.6 MB/s at 0.48 s blocks) |
| Sign, per vote (Apple M4 Max; ed25519: Go stdlib, ML-DSA: circl v1.6.5; CometBFT v0.40 pins v1.6.3, re-measure on it) | ~14 µs | ~0.2 ms average (varies with rejection sampling) |
| Verify, per signature | ~30 µs | ~31 µs |

- CPU is not the problem. Each validator signs ~3 times per height (~0.65 ms total), and verification costs about the same as ed25519.
- Storage and bandwidth are. Commit data grows ~32× (signature bytes alone ~52×), and the blockstore keeps each commit three times; signatures don't compress.
  - Size block store pruning (`min-retain-blocks`), archive-node disk, snapshots and state-sync.
  - `HistoricalInfo` grows app state on every block and is kept forever in versiondb. Any non-zero `historical_entries` keeps the per-block write; a smaller value only bounds retained entries. ibc-go v11 doesn't read it.
    - Its only runtime consumer is ethermint's `GetHashFn` `BLOCKHASH` fallback when the EIP-2935 header hash is missing (`x/evm/keeper/state_transition.go`), plus the staking `HistoricalInfo` query.
    - Decided: the v1.9 upgrade handler sets `historical_entries = 0` and prunes the existing entries when EIP-2935 covers the last 256 blocks, and leaves the param alone otherwise (§4 step 4). Mainnet and testnet have the contract with an 8191-block window.
    - Live check (§6): the EIP-2935 contract is deployed on mainnet and testnet with an 8191-block window, so the fallback is never hit there and `historical_entries = 0` (today 10000) is viable. Confirm on testnet first; check any other network (devnets, testground) the same way.
    - Measure memiavl/versiondb growth and block time on testground.
- CometBFT v0.40 already reserves ML-DSA-sized commit space in `MaxDataBytes` for every validator (§7), so enabling the key type does not shrink packing capacity further.
- Duplicate-vote evidence grows to ~7 KB per piece; check `consensus_params.evidence.max_bytes`.
- Consensus addresses stay `sha256(pubkey)[:20]`, so ethermint's `miner`/`ValidatorAccount` RPCs need no format change.

### 11.4 Blockers and prerequisites

| Item | Status | Needed |
|---|---|---|
| Ethermint coinbase/`miner` lookup across rotation (§2) | Breaks on rotation today | Must ship in the v0.55 upgrade, before any rotation |
| IBC counterparties | Each must run CometBFT v0.38.26+ (ML-DSA verification backported) or v0.40+ to verify ML-DSA headers in `07-tendermint`; v0.39.x has no backport. Otherwise headers fail verification, packets stop, and the client eventually expires. Each Cronos `MsgUpdateClient` grows to ~154 KB (commit + 29 × 1.95 KB pubkeys). | Live mainnet counterparties (§6, CometBFT version from their public `node_info`, 2026-10-29): Cronos POS 0.38.22, Cosmos Hub 0.38.25, Akash 0.38.19, Celestia 0.38.17 (celestia-core), Juno 0.38.22. **None can verify ML-DSA yet.** Coordinate their upgrades first; Cronos POS (chain-main, our fork) is the one we control. Check their `max_tx_bytes`, tx-size gas and fees, and the relayer `max_tx_size`. |
| Hermes relayer | Verifies headers with tendermint-rs (now `cometbft/tendermint-rs`), whose `PublicKey` has only Ed25519 and Secp256k1. No ML-DSA code or open PR in tendermint-rs or Hermes (checked 2026-10-29). | Upstream support or a relayer that uses the Go light client. Without it, IBC relaying from Cronos stops once ML-DSA validators sign. Re-check at rollout time. |
| Remote signers / KMS | tmkms keyring has only ed25519/ecdsa, no open ML-DSA PR. Horcrux is archived (2025-03), so it will never support ML-DSA. CometBFT's privval socket protocol carries the new key type, but each signer must implement it. | Survey how Cronos validators sign. Validators on unsupported signers can't rotate without dropping their signer. |
| Monitoring, explorers, indexers | Keyed by consensus address and ed25519 pubkey format | Handle `rotate_cons_pubkey` / `apply_cons_pubkey_rotation` and `cometbft/PubKeyMlDsa65` |
| Rotation fee | Set to `1stake` by the v1.9 upgrade (§4 step 4) | Each rotating operator holds ≥ 1 liquid `stake` (25 of 29 hold none today) |

### 11.5 Rollout

1. v0.55 upgrade ships with `pub_key_types = ["ed25519"]` and the §2 ethermint fix.
2. Testground (16-node) with all validators on ML-DSA-65: block time, gossip bandwidth, blockstore and memiavl/versiondb growth, state-sync and snapshot size, TPS against the ed25519 baseline. Add a mixed set and a rotation under load.
3. Testnet: gov proposal to enable `ml_dsa_65`; rotate validators in waves; relay to a counterparty on CometBFT v0.38.26+ or v0.40+ with a Go-light-client relayer (Hermes can't parse ML-DSA validator sets, §11.4).
4. Clear §11.4 blockers for mainnet (IBC counterparties, relayer, signers).
5. Mainnet: gov proposal to enable the key type, then rotations in waves (e.g. ≤ 20% of voting power per wave; consensus needs > 2/3 online, so a wave that misses blocks must stay well under 1/3), each wave confirmed in `/validators` before the next.

Rollback: there is no fast rollback.

- A validator can rotate back to ed25519 only after its 28-day window, and only to a **fresh** ed25519 key; the old address stays locked (`ErrConsensusPubKeyInRotationHistory`).
- The key type can be removed from `pub_key_types` only after no validator uses it.
- Once any ML-DSA validator is active, a software rollback to a pre-v0.55 binary is impossible, because older binaries can't parse the validator set.

## 12. Alignment with the v1.9 roadmap proposal (October 2026)

The roadmap slide ("Cronos v1.9: Roadmap Proposal", 01/02) puts this migration in v1.9, next to go-ethereum v1.17, Go 1.26 and cronos-store work, plus a post-quantum track. This section maps each item onto the plan and notes where the plan adds something or corrects the slide.

### 12.1 Dependencies and integration

| Roadmap item | Plan | Adds or corrects |
|---|---|---|
| Cosmos SDK v0.55 + CometBFT v0.40 | §1–§9 | The slide calls it an easy win. Three items must clear first: the upgrade-height halt (§4.3, ≈1% chance, fix in our CometBFT fork), the EVM coinbase gap during key rotation (§2), and the undecodable `ParameterChangeProposal`s (§4.2). |
| go-ethereum v1.17.x (Amsterdam not activated) | §5 (ethermint#1136) | ethermint#1136 targets the crypto-org-chain fork `43022f1730ed` (the fork's stateful-precompile interface ported onto v1.17.10), still on Osaka rules. Two **state-machine-breaking** changes take effect at the upgrade height and need tests: (1) a stateful precompile reached by `CALL` inside a static context now gets `readonly = true`. Cronos registers no stateful precompiles (`app.go:802` passes an empty `CustomContractFn` list since #1986), so this changes nothing on Cronos; cover it in ethermint with a mock precompile test; (2) EIP-6780 SELFDESTRUCT moves balance in the interpreter, so bank event order changes (indexers). The interpreter rewrite can't be checked by unit tests alone. Because the v1.9 app can't execute pre-upgrade blocks, do the differential replay before the SDK port: build a v1.8 binary with only ethermint#1136 swapped in (plus the minimal Cronos compile fixes for the go-ethereum v1.17 API, e.g. `core.Message`, `vm.GasBudget`), replay a mainnet block range from a snapshot, and compare app hashes and receipts with v1.8. Only blocks that hit change (2) may differ. "90% RPC equivalence" means some RPC responses differ; list the gaps in the operator guide. |
| Go 1.26 toolchain | §7 (Go 1.26.10) | The fork needs 1.26.10, not just 1.26. Cheaper cgo calls may change the 2026-02 benchmark where pure-Go secp256k1 beat CGO on arm64; re-run the CGO vs pure-Go secp256k1 comparison (the 2026-02 benchmark isn't in the repo) before choosing the build mode. |
| cronos-store dependencies | §1 (cronos-store row) | Not just a go.mod bump; see the notes below the table. |
| libp2p node auto discovery | — | Struck from the slide; out of v1.9 scope. |

cronos-store notes:

- Invariant: memiavl's committed version must never exceed versiondb's (`MaxVersionStoreLoader` loads memiavl at the versiondb version, and app.go refuses to start when versiondb lags). The fsync change alters the commit order, so crash-test both orders: kill between the memiavl commit and the versiondb write, and between the versiondb write and the memiavl commit, then restart.
- Dropping `iavl` removes only memiavl's import. versiondb (`tsrocksdb`, changeset client) and SDK `store/v2` still pull in `iavl v1.2.8`. memiavl uses iavl for ICS23 proof construction (`PathToLeaf`, `ProofInnerNode`) and its cache. Add tests that proofs stay byte-for-byte compatible (`abci_query` with prove, IBC membership proofs a counterparty accepts), plus snapshot export/import and state-sync.

### 12.2 Post-quantum track

| Roadmap bucket | Item | Plan status |
|---|---|---|
| Included with the upgrade | ML-DSA-65 validator and account keys, opt-in | Validator keys: ships with v0.55, `pub_key_types` stays `["ed25519"]` (§3.6, §11). Account keys: ethermint's `DefaultSigVerificationGasConsumer` falls through to the SDK, which charges `SigVerifyCostMlDsa65`, so ML-DSA Cosmos txs work. They can't sign EVM txs, and `cronosd keys add --algo ml_dsa_65` needs the algorithm added to Cronos's keyring options. Add one integration test that signs a Cosmos tx with an ML-DSA account. |
| | In-place consensus key rotation | Ships with v0.55. Needs the §2 coinbase fix, the §4.1 pool fix and `key_rotation_fee = 1stake` (§4 step 4). |
| | IBC light clients verify PQ counterparties | Inbound only: with CometBFT v0.40, Cronos's `07-tendermint` clients can verify counterparties that adopt ML-DSA. Outbound (counterparties verifying Cronos) still needs their upgrades (§11.4). |
| Ship on top in v1.9 | Hybrid X25519 + ML-KEM-768 keys in `x/e2ee` (blocklist) | See §12.3. |
| | Crypto inventory and agility pass | No plan impact. |
| Testnet only, or later | ML-DSA-44 verify precompile for EIP-7702 accounts (EIP still Draft) | A new precompile is consensus code, so the mainnet binary must not activate it: gate it by chain-id or an activation param that stays off on mainnet. The precompile address and interface may change before the EIP is final. |
| | PQ consensus keys on mainnet | Matches §11 (separate phase). **Correct the slide's "~330 KB per block":** that's the SDK docs' example for 100 validators and 6 s blocks. For Cronos (29 validators, 0.48 s blocks) it's ~97 KB per commit, ~291 KB per height in the blockstore (stored 3×), ~52 GB/day and ~0.6 MB/s per peer link (§11.3). |
| | Ethereum L1 PQ not before 2029 | No plan impact. |

### 12.3 Hybrid ML-KEM keys in `x/e2ee`

- **Validation is consensus code.** `ValidateRecipientKey` (`x/e2ee/types/msg.go:21`, also used by genesis validation in `types/keys.go:40`) accepts only `age.ParseX25519Recipient`. Accepting hybrid keys means also trying `age.ParseHybridRecipient` (`age1pq1…`; age v1.3.1 is already in `go.mod`). That changes which txs are valid, so it can only go live at the v1.9 upgrade height.
- **Other X25519-only code:**
  - the CLI commands `generate` (`client/cli/generate.go:38`, needs a hybrid identity generator), `pubkey` (`pubkey.go:41`), `encrypt` (`encrypt.go:52`), `encrypt-to-validators` (`encrypt_to_validators.go:72`, the admin blocklist flow that hits the mixed-recipient refusal) and `decrypt` (`decrypt.go:56`);
  - the node, which loads exactly one identity with `age.ParseX25519Identity` (app.go:415-433);
  - `ProposalHandler.Identity`, a single `age.Identity` passed to `age.Decrypt` (`app/proposal.go:225`).

  All of these need code changes to handle hybrid keys and to hold both identities at once.
- **No mixed recipients.** age v1.3.1 refuses to encrypt one file to post-quantum and classic recipients together (`incompatible recipients: can't mix post-quantum and classic recipients`, `age.go:134`, message at `:227`). Each address holds one key, and `RegisterEncryptionKey` overwrites it (`keeper.go:46`). So once the first validator registers a hybrid key, the admin can't encrypt a blocklist that includes every validator: hybrid for all is impossible until everyone has switched, and leaving a validator out means it proposes with no blocklist and accepts txs the others reject. **Blocklist updates are stuck from the first registration to the last.**
- **Decided fix (§10.9):** in v1.9, let `x/e2ee` store a hybrid key next to the X25519 key for each address (state and proto change, at the upgrade height). Then:
  1. validators register hybrid keys at their own pace while the admin keeps encrypting to X25519;
  2. once every validator has a hybrid key, the admin switches the blocklist to hybrid in one update;
  3. validators drop their X25519 identity afterwards.
- **Alternatives:**
  - freeze blocklist changes, except an emergency procedure, during one short coordinated registration window;
  - have every validator register a hybrid key in the blocks right after the upgrade.

  Both depend on every validator acting on time.
- **Size.** A hybrid recipient string is ~1.95 K characters (1,216-byte key in bech32), against ~62 for X25519. Each recipient stanza carries a 1,120-byte encapsulation (~1.5 KB as base64 text in the age header), so a blocklist encrypted to 29 validators grows by about 43 KB. Check message and tx size limits for storing the blocklist and registering keys.
