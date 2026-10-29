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
cometbft#71 ─► cosmos-sdk#1888 ─► #1889 ─┬──────────────► ethermint v0.55 ─┬─► cronos
                                         └─► cronos-store ─────────────────┤
ibc-go (pinned, see below) ─────────────────► ethermint v0.55, cronos      │
pending ethermint PRs (§5) ─────────────────► ethermint v0.55              │
pending cronos PRs (§5) ───────────────────────────────────────────────────┘
```

| Repo | State today | Needed |
|---|---|---|
| crypto-org-chain/cometbft | #71 open (v0.40.0 + fork #51 app-mempool fix + lp2p queue cap). Fork #50, #54, #64 already upstream. All commits in Cronos's current pin are covered. | Fix the `MedianTime` upgrade-height risk (§4.3). Merge #71, decide on #72. Repin SDK to the merge commit. |
| crypto-org-chain/cosmos-sdk | #1888 open on `release/v0.55.x`; #1889 stacked. | Merge both. #1889 changes the staking queue index and the upgrade handler (§4), so land it before Cronos pins. |
| crypto-org-chain/cronos-store | Pins SDK v0.54.3, CometBFT v0.39.3. `store/v2` stays `v2.0.0` in v0.55. memiavl already supports `StoreUpgrades.Deleted`. | go.mod bump only (SDK, CometBFT, Go 1.26.10); run memiavl/versiondb tests; tag. Only Cronos depends on it, so this runs in parallel with the ethermint port. |
| cosmos/ibc-go | v11.2.0 still pins SDK v0.54.0. `main` (still `/v11`) moved to SDK v0.55.0 + CometBFT v0.40.0 in cosmos/ibc-go#9011 (merge commit `ae841d41a589`). `main` after that carries state-machine-breaking commits in no tag (e.g. #9033 future-revision packet timeouts, #9080 `RecoverClient`). | Prefer a reviewed v11 tag with #9011. If none ships in time, pin exactly `ae841d41a589` and review its delta from v11.1.0 for state-machine changes. Don't track `main`. |
| crypto-org-chain/ethermint | No v0.55 branch. Main pins SDK fork v0.54. | Full port (§2) after its pending PRs (§5). Tag a new minor. |

#72 content (for the §10 decision): recover the AppMempool from app errors and panics; drop failed-CheckTx txs from the seen cache after `check_tx_retry_delay`; throttled logs; a `failed_txs` metric; zero `seen_cache_size` / `check_tx_retry_delay` rejected at config validation.

## 2. Ethermint port (critical path)

Ethermint carries most of the EVM-specific breakage. Start it once its pending PRs (§5) have landed, in its own PR on ethermint main.

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
  - `x/evm/keeper/grpc_query.go` `ValidatorAccount` and `rpc/backend` (`getValidatorAccount`, blocks, chain info, fee history) look up the `miner` the same way. For old blocks, fall back to `ValidatorByHistoricalConsAddr`.
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
- **`baseapp.TxSelector`**: `SelectTxForProposal(..., memTx sdk.Tx, txBz []byte, gasWanted uint64)`. `app/proposal.go` `ExtTxSelector` uses `feeTx.GetGas()` today. **Decision needed:** keep the declared gas limit or switch to the ante-reported `gasWanted` like upstream (`txGasForBlockAccounting`: ante `GasWanted`, falling back to `GetGas()` when 0).
  - Not consensus-critical for Cronos: `ProposalHandler.ProcessProposalHandler` replaces the SDK default and only runs the blocklist check, no block-gas check. It only changes block packing. Upstream's default `ProcessProposal` does enforce gas, so this would matter if Cronos ever went back to it.
- **`ProposalTxVerifier.ProcessProposalVerifyTx`** now returns `(sdk.Tx, gasWanted uint64, error)`. Cronos doesn't implement it; recompile only.
- **`app/mempool/reap.go`** reads `feeTx.GetGas()` for reap gas; same decision applies.
- **Staking hooks**: `StakingHooks` adds `AfterValidatorConsKeyUpdated`. Cronos only chains SDK `distr` + `slashing` hooks, so no change unless a custom hook is added.

### 3.3 New required wiring

- `maccPerms[stakingtypes.KeyRotationFeePoolName] = {authtypes.Burner}`. The staking keeper panics at construction without it.
- `BlockedAddrs()` builds from `maccPerms`, so the new account is blocked for bank sends automatically. EVM value transfers don't check `BlockedAddrs()` (only the Cronos bank precompile does), so plain EVM transfers to the pool still work after the upgrade, as they do for every other module account. That's harmless here: rotation only sends and burns the fee itself, so extra funds just stay stuck in the pool. §4.1 covers the pre-upgrade case.

### 3.4 Stricter validation to cover in tests

No code change expected, but each can reject txs that v0.54 accepted:

- `SetPubKeyDecorator` rejects a tx whose `SignerInfo` count differs from its signer count (`ErrTxDecode`, cosmos/cosmos-sdk#26573). Covers Cosmos txs and EIP-712 txs through ethermint's Cosmos ante chain.
- Multisig: bit-array size must match the key set (#26515), and nested multisig signatures are capped at depth 2 and 32 total (`flattenSignatures`, `ErrTooManySignatures`). Check whether any Cronos account uses nested multisig.
- `secp256k1.PubKey` unmarshal requires tag byte `0x02`/`0x03` (#26529). Ethermint's `ethsecp256k1` is a separate type and unaffected.
- `Any` recursion cap in unknown-field checks drops from 10,000 to 64 (#26587). Check nested `authz.MsgExec` payloads.

### 3.5 Optional / keep-as-is

- **Block-STM**: Cronos already uses `baseapp/blockexec` + `SetDisableBlockGasMeter(true)` (backported in the v0.54 fork). Recompile only.
- **`contrib/x/crisis`** still exists in v0.55. Keeping it is fine; removing it would add `crisis` to the store deletions. Recommend keep for this hop.
- **ML-DSA-65 / secp256k1eth consensus keys**: ship the code, keep `pub_key_types = ["ed25519"]` at the upgrade. ML-DSA-65 migration is a separate phase (§11).
- **Behavior changes with no code change**: begin/end-block reward withdrawals to blocked addresses fall back to the owner, then the community pool (#26406); `x/authz` prunes at most 200 expired grants per block (#26588).

## 4. Upgrade handler and store migrations

Next plan name: assumed `v1.9` (`docs/migrations/v1.8-to-v1.9.md` already exists on main; #2216 extends it).

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
2. `RunMigrations`: `x/staking` 5 → 6 (adds `key_rotation_fee`, default `1000000` of bond denom `stake`; `Params.Validate` requires `fee.denom == bond_denom`), `x/auth` 6 → 7 (adds `SigVerifyCostMlDsa65`). v0.55 has no migrations left for gov, bank, slashing, distribution or crisis, so those modules must already be at their v0.54 versions (§6).
3. **Staking queue index.** With #1889 merged, `Migrate5to6` recomputes the earliest-time gate keys (`0x44`–`0x46`) and `PopulateQueuePendingSlots` is a deprecated alias: drop the call. Without #1889, keep the call (v1.8 already ran it; rerun is safe).
4. Set Cronos-specific params if the defaults don't fit (e.g. `key_rotation_fee`, §7).
5. Drop the v1.8-only steps (IBC prune, `CroBridgeContractAddresses`) once v1.8 is the floor.

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

CometBFT v0.40 (cometbft#5901) makes `MedianTime` skip `Nil` precommits; v0.39.3 skipped only `Absent`. `validateBlock` requires `block.Time == MedianTime(block.LastCommit)`, and the change has no height gate.

- The upgrade-height block H is proposed and committed by v0.39 binaries. CometBFT saves block H and its seen commit before `FinalizeBlock`, then the SDK halts in `PreBlock`.
- On restart, the v0.40 binary replays H through `Handshaker.replayBlock` → `ApplyBlock` → `validateBlock` (normal consensus uses `ApplyVerifiedBlock`, but the handshake doesn't), using the new rule on H's `LastCommit` (the commit for H-1).
- If that commit contains a `Nil` precommit that moves the weighted median, validation fails with `invalid block time` on every node, and the chain halts.
- **Measured on mainnet** (300 blocks ending at 102,782,600, 2026-10-29):
  - 6 commits (2%) carried one `Nil` precommit.
  - In 2 of them (heights 102,782,543 and 102,782,575, ≈0.7% of blocks) the v0.40 median differs from the header time, by 0.15 ms and 0.65 ms.
  - The v0.39 recomputation matched the real header time in all 8 blocks checked.
  - So an unpatched upgrade has a halt chance on the order of 1% (2 of 300 sampled blocks; 95% interval ~0.1–2.4%). The rate depends on how often validators precommit nil, so it is not constant.
  - Historical blocks with the same property can't be replayed by v0.40 either; at this rate that's hundreds of thousands of the ~102.8M blocks.
- The old binary can't recover it either, because it halts again at H. Only a patched v0.40 binary gets the chain going.
- The same rule makes v0.40 unable to blocksync or replay any pre-upgrade block whose commit has `Nil` precommits. Nodes syncing from genesis or a pre-upgrade snapshot need the old binary up to H (cosmovisor).

Fix in the fork (cometbft#71 or a follow-up) before scheduling:

- **Preferred:** revert cometbft#5901 in the fork. Cronos doesn't need `MedianTime` to match `VerifyCommit`'s tally, and a revert needs no activation height.
- **Fallback:** keep #5901 behind an activation height hard-coded in the binary per chain-id, applied identically in `MakeBlock` and `validateBlock`.
- **Never** use an operator-configured height. Nodes with different values split at the activation height.

Raise it upstream too. Test as a CometBFT fork unit test: build block H whose `LastCommit` has a `Nil` precommit that shifts the median, then run the handshake replay with app = state = H-1 and store = H. Assert that the reverted or gated rule accepts it and that unpatched v0.40 rejects it. Producing that exact commit in pystarport isn't controllable.

## 5. Pending PRs: land before the migration branch

All three Cronos PRs touch `app/mempool`, which is where most v0.55 mempool interface churn lands. Ethermint PRs touch code the port rewrites.

| PR | Overlap with v0.55 work | Recommendation |
|---|---|---|
| cronos#2216 commit priority + RPC in-flight cap (ready) | `app/app.go`, `app/mempool/manager.go`, `cmd/cronosd/config`, `test_upgrade.py`, extends `docs/migrations/v1.8-to-v1.9.md` | Merge first. Its upgrade test and migration doc are the base the v0.55 upgrade extends. |
| cronos#2239 pool tx lookup for tx replacement (draft) + ethermint#1130 | `app/app.go`, `app/mempool/manager.go`, `app/proposal.go` (`ExtTxSelector`), `go.mod`, `test_upgrade.py` | Land together before the ethermint port starts, or both sides get redone. |
| cronos#2159 recheck per signer group (draft) | `app/mempool/manager.go`, `exec.go`, `scheduler.go`, recheck tests | Land before the branch cut if ready; otherwise rebase onto `feat/sdk-v0.55` and switch its `SelectBy` usage to `PooledTx`. |
| ethermint#1118 skip historical state in block RPCs | `rpc/backend` validator lookups (also touched by §2 key rotation) | Land before the port; then apply the §2 rotation fix on top. |
| ethermint#1120 compact EVM storage + sweep zeroed slots | Adds evm v8 → v9 migration, params proto change, and a BeginBlock sweep (10k slots/block for ~12.5k blocks) | Decide whether it ships in the same upgrade. If yes, include it in §4, §6 and §8 (migration runtime, sweep load on block time). |
| ethermint#1136 go-ethereum v1.17 (draft) | `go.mod`, `x/evm/types/msg.go` (§2 textual), `x/evm/keeper/utils.go` and `grpc_query.go` (§2 rotation fix), other `x/evm/keeper` and statedb files | Keep out of this upgrade unless already planned; it would compound risk. Expect conflicts with the port when it rebases. |

Cut `feat/sdk-v0.55` from main after cronos#2216 and cronos#2239 land.

## 6. Live-chain checks

Probed 2026-10-29 through the public REST endpoints (`rest.cronos.org`, `rest-t3.cronos.org`) and EVM JSON-RPC. CometBFT RPC and `node_info` sit behind Cloudflare/403 and weren't used. Mainnet height ~102.78M; both chains run v1.8.

| Check | Required | Mainnet | Testnet |
|---|---|---|---|
| Module versions | auth 6, staking 5, gov 5, bank 4, slashing 4, distribution 3, crisis 2, mint ≥ 2; evm ≥ 6, feemarket ≥ 4, cronos ≥ 2 | auth 6, staking 5, gov 5, bank 4, slashing 4, distribution 3, crisis 2, mint 3; evm 8, feemarket 4, cronos 2 ✅ | identical ✅ |
| Historical `ParameterChangeProposal`s (§4.2) | none, or vendor the types | 3 (ids 3, 4, 8) ❗ | 13 ❗ |
| Open gov proposals | none at upgrade | none ✅ | none ✅ |
| `key_rotation_fee_pool` account (§4.1) | absent | absent, zero balance ✅ | absent, zero balance ✅ |
| `block.max_bytes` / `evidence.max_bytes` (§7) | fits v0.40 commit reservation | 1,048,576 / 150,000 ✅ (tx space −93 KB: ~1,044 → ~950 KB typical, ~894 → ~800 KB with max evidence) | same ✅ |
| `validator.pub_key_types` | `["ed25519"]` | `["ed25519"]`; 29 bonded, all ed25519 | `["ed25519"]`; 29 bonded |
| Staking params | — | unbonding 28 d, `max_validators` 30, `historical_entries` 10000, bond denom `stake` | unbonding 28 d, `max_validators` 35, same otherwise |
| Evidence max age | — | 210,000 blocks / 1 day | 403,200 blocks / 28 days |
| EIP-2935 history contract (§11.3) | deployed, window ≥ 256 | deployed, `prague_time` 0, window 8191 (default) ✅ | deployed, window 8191 ✅ |
| Block interval | — | ~0.48 s (285 blocks in 137.6 s), ~179k blocks/day | — |
| `Nil` precommits that move the median (§4.3) | 0 | 2 of 300 blocks (≈0.7%) ❗ | not sampled |
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
- **Key rotation** is on for every v0.55 chain once wired. Decide the `key_rotation_fee` (default `1000000stake`) and whether operators need a runbook. Indexers keyed by consensus address must handle rotation events (`rotate_cons_pubkey`, `apply_cons_pubkey_rotation`).
- **Block selection** may change if `ExtTxSelector` switches to ante `gasWanted` (§3.2).
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
  - `ExtTxSelector` gas source;
  - ethermint `GetParams` with the params key missing, and coinbase lookup across a key rotation.
- Integration (pystarport):
  - full suite;
  - `test_upgrade.py` v1.8 → v1.9 with a pre-funded pool address, a historical params proposal, and versiondb enabled (the §4.3 case is a CometBFT fork unit test);
  - `test_app_mempool.py` for the CheckTx error code change;
  - a key rotation with EVM txs around the apply height.
- Upgrade package (`integration_tests/configs/upgrade-test-package.nix`): add a `released1_8` entry and rename `current` from `"v1.8"` to `"v1.9"`.
- Mainnet-state dry run: in-place upgrade on a mainnet snapshot (memiavl + versiondb), then `cronosd export` and historical gov/tx queries.
- Performance: testground benchmark vs v1.8 baseline (block time, admission, max block). Watch CometBFT v0.40 block size accounting and gas-selection changes.
- Sims: `TestAppImportExport`, `TestAppStateDeterminism` after `x/params` removal.

## 9. Delivery sequence

1. Fix §4.3 in the CometBFT fork; merge cometbft#71 (+ #72 decision) → repin SDK.
2. Merge cosmos-sdk#1888, #1889 → tag fork v0.55.x.
3. §6 checks ran 2026-10-29. Re-run the pool-address and open-proposal checks right before submitting the upgrade proposal.
4. In parallel:
   - cronos-store bump + tag; ibc-go pin (§1);
   - land cronos#2216, cronos#2239 + ethermint#1130, ethermint#1118; decide ethermint#1120; land or schedule cronos#2159 (§5).
5. Ethermint v0.55 port (§2) → tag.
6. Cronos PR on `feat/sdk-v0.55`: deps + §3 + legacy types (§4.2), then the upgrade handler (§4) in a second commit.
7. §8 tests, testnet upgrade, operator guide (`docs/migrations/`), mainnet proposal.
8. After the upgrade is stable: ML-DSA-65 phase (§11).

## 10. Open decisions

1. Target release: v1.9 alongside #2216, or a separate v1.10?
2. `ExtTxSelector`/reap: declared gas limit or ante `gasWanted`?
3. `key_rotation_fee` value for Cronos staking.
4. Keep `contrib/x/crisis` for this hop (recommended) or remove it now.
5. Take cometbft#72 in this release or later.
6. Ship ethermint#1120 (evm v9 storage migration) in the same upgrade or separately.
7. CometBFT `MedianTime` change (§4.3): revert cometbft#5901 in the fork (recommended) or gate it with a hard-coded per-chain height.
8. ML-DSA-65 (§11): go/no-go after the Phase 2 measurements, and whether the storage/bandwidth cost is worth it before quantum attacks on ed25519 are practical.

## 11. Phase 2: validator migration to ML-DSA-65 consensus keys

Goal: post-quantum consensus security, i.e. every validator signing with `ml_dsa_65` (NIST FIPS 204). This is **not** part of the v0.55 upgrade handler. v0.55 + CometBFT v0.40 only make it possible; turning it on is a governance and operator rollout after the upgrade is stable.

### 11.1 Scope: what it does and doesn't protect

- Protects: consensus votes, commits, and therefore light-client/IBC verification of Cronos headers.
- Does not protect:
  - user accounts: EVM accounts stay `eth_secp256k1`. SDK ML-DSA account keys (`--algo ml_dsa_65`, `SigVerifyCostMlDsa65`) work only for Cosmos txs and can't sign EVM txs;
  - p2p node keys (stay ed25519);
  - validator operator keys, which still sign `MsgRotateConsPubKey` and staking txs.

### 11.2 Mechanics

1. **Enable the key type** with a gov proposal to `x/consensus` `MsgUpdateParams` (`tx consensus update-params-proposal`). The proposal takes the **entire** params (block, evidence, validator, abci). Fetch the current `cronosd q consensus params`, change only `validator.pub_key_types` to `["ed25519", "ml_dsa_65"]`, and submit the full JSON; a partial JSON zeroes the block and evidence params. This must happen before any rotation. Never remove `ed25519` while any validator uses it: the next voting-power update for that validator fails validation and halts the chain.
2. **Each validator rotates** with `tx staking rotate-cons-pub-key <new ml_dsa_65 pubkey>`:
   - Generate the key offline (`cronosd init --consensus-key-algo ml_dsa_65` in a scratch home).
   - Timing: if the tx executes at height h, the validator still signs with the old key at h and h+1, and the new key must sign from h+2. Swap after block h+1 commits and before h+2: replace `priv_validator_key.json` and restart the validator node (or start an already-synced standby node holding only the new key and stop the old one). Missing the window costs missed blocks, not a fault.
   - At the swap, remove the old key file from that host. Never let the old key run on two nodes, and never copy it to a second node. Two signers on one key is a double sign and tombstones the validator.
   - One rotation per validator per unbonding period (28 days on Cronos), so a bad rotation can't be fixed for 28 days.
   - The operator account pays `key_rotation_fee` (burned) from its **unbonded** `stake` balance, plus gas in CRO. Gas covers tx size (~10 gas/byte, ~22k gas for the ~2 KB pubkey) and several ~2 KB state writes (30 gas/byte), and fees go through ethermint's dynamic fee checker. Estimate on testnet; treat `--gas-adjustment 1.5` as a floor.
   - The key is a raw keypair with no mnemonic, so back up the key file itself.
3. **Done** when every validator reports `cometbft/PubKeyMlDsa65` in `/validators`. At only 2/3 there is no fault margin: an attacker who can forge the remaining ed25519 keys (just under 1/3) breaks safety with one more Byzantine validator, and light clients trust 1/3.
4. **Residual risk after rotation:** evidence against a rotated-away ed25519 address stays admissible until both `evidence.max_age_num_blocks` and `max_age_duration` have passed (mainnet: 210,000 blocks ≈ 28 h at 0.48 s, and 1 day; testnet: 28 days). Until then, a quantum attacker could forge double-sign evidence with the old key and slash or tombstone the validator.

### 11.3 Cost for Cronos

Sizes: pubkey 1,952 B vs 32 B; signature 3,309 B vs 64 B. Each commit vote also carries ~40 B of address, timestamp and flag. Cronos numbers use the measured mainnet inputs (§6): 29 bonded validators, ~0.48 s blocks, ~179k blocks/day:

| | ed25519 (today) | ML-DSA-65 (all 29 rotated) |
|---|---|---|
| One commit (29 votes) | ~3.0 KB | ~97 KB |
| Blockstore commit data per height (stored 3×: block `LastCommit`, block-commit key, seen commit) | ~9 KB | ~291 KB |
| Blockstore commit data per day / year (before pruning) | ~1.6 GB / ~0.6 TB | ~52 GB / ~19 TB |
| App state: staking `HistoricalInfo` written every block (29 validator pubkeys) | ~0.9 KB | ~57 KB (~10 GB/day into memiavl WAL and versiondb) |
| Bandwidth per height per peer link (58 votes + block `LastCommit`, both with ~40 B per-vote overhead) | ~9 KB | ~291 KB (~0.6 MB/s at 0.48 s blocks) |
| Sign, per vote (Apple M4 Max; ed25519: Go stdlib, ML-DSA: circl v1.6.5 as CometBFT uses it) | ~14 µs | ~0.2 ms average (varies with rejection sampling) |
| Verify, per signature | ~30 µs | ~31 µs |

- CPU is not the problem. Each validator signs ~3 times per height (~0.65 ms total), and verification costs about the same as ed25519.
- Storage and bandwidth are. Commit data grows ~32× (signature bytes alone ~52×), and the blockstore keeps each commit three times; signatures don't compress.
  - Size block store pruning (`min-retain-blocks`), archive-node disk, snapshots and state-sync.
  - `HistoricalInfo` grows app state on every block and is kept forever in versiondb. Any non-zero `historical_entries` keeps the per-block write; a smaller value only bounds retained entries. ibc-go v11 doesn't read it.
    - Its only runtime consumer is ethermint's `GetHashFn` `BLOCKHASH` fallback when the EIP-2935 header hash is missing (`x/evm/keeper/state_transition.go`), plus the staking `HistoricalInfo` query.
    - If EIP-2935 hashes cover the last 256 blocks on every network, set `historical_entries = 0` by gov param update; that change is consensus-affecting. Otherwise keep it ≥ 256 and accept the write.
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
| Rotation fee | Default `1000000stake` | Set in the v0.55 upgrade (§10.3) |

### 11.5 Rollout

1. v0.55 upgrade ships with `pub_key_types = ["ed25519"]` and the §2 ethermint fix.
2. Testground (16-node) with all validators on ML-DSA-65: block time, gossip bandwidth, blockstore and memiavl/versiondb growth, state-sync and snapshot size, TPS against the ed25519 baseline. Add a mixed set and a rotation under load.
3. Testnet: gov proposal to enable `ml_dsa_65`; rotate validators in waves; relay to a counterparty on CometBFT v0.38.26+ or v0.40+ with a Go-light-client relayer (Hermes can't parse ML-DSA validator sets, §11.4).
4. Clear §11.4 blockers for mainnet (IBC counterparties, relayer, signers). Decide `historical_entries` (§11.3) after checking EIP-2935 coverage.
5. Mainnet: gov proposal to enable the key type, then rotations in waves (e.g. ≤ 20% of voting power per wave; consensus needs > 2/3 online, so a wave that misses blocks must stay well under 1/3), each wave confirmed in `/validators` before the next.

Rollback: there is no fast rollback.

- A validator can rotate back to ed25519 only after its 28-day window, and only to a **fresh** ed25519 key; the old address stays locked (`ErrConsensusPubKeyInRotationHistory`).
- The key type can be removed from `pub_key_types` only after no validator uses it.
- Once any ML-DSA validator is active, a software rollback to a pre-v0.55 binary is impossible, because older binaries can't parse the validator set.
