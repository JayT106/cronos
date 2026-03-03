package app

import (
	"fmt"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// mockProposalVerifier implements baseapp.ProposalTxVerifier using a bidirectional
// tx↔bytes map so test code can register test transactions and later identify them
// by the bytes returned in the proposal.
type mockProposalVerifier struct {
	txToBytes map[sdk.Tx][]byte
	bytesToTx map[string]sdk.Tx
}

func newMockProposalVerifier() *mockProposalVerifier {
	return &mockProposalVerifier{
		txToBytes: make(map[sdk.Tx][]byte),
		bytesToTx: make(map[string]sdk.Tx),
	}
}

func (m *mockProposalVerifier) registerTx(tx sdk.Tx, bz []byte) {
	m.txToBytes[tx] = bz
	m.bytesToTx[string(bz)] = tx
}

func (m *mockProposalVerifier) PrepareProposalVerifyTx(tx sdk.Tx) ([]byte, error) {
	bz, ok := m.txToBytes[tx]
	if !ok {
		return nil, fmt.Errorf("mockProposalVerifier: unknown tx")
	}
	return bz, nil
}

func (m *mockProposalVerifier) ProcessProposalVerifyTx(txBz []byte) (sdk.Tx, uint64, error) {
	tx, ok := m.bytesToTx[string(txBz)]
	if !ok {
		return nil, 0, fmt.Errorf("mockProposalVerifier: unknown txBz")
	}
	return tx, 0, nil
}

func (m *mockProposalVerifier) TxDecode(txBz []byte) (sdk.Tx, error) {
	tx, ok := m.bytesToTx[string(txBz)]
	if !ok {
		return nil, fmt.Errorf("mockProposalVerifier: unknown txBz")
	}
	return tx, nil
}

func (m *mockProposalVerifier) TxEncode(tx sdk.Tx) ([]byte, error) {
	return m.PrepareProposalVerifyTx(tx)
}

// newIntegrationCtx returns an sdk.Context with maxBlockGas embedded in ConsensusParams.
func newIntegrationCtx(maxBlockGas int64) sdk.Context {
	return sdk.NewContext(nil, cmtproto.Header{}, false, nil).
		WithConsensusParams(cmtproto.ConsensusParams{
			Block: &cmtproto.BlockParams{MaxGas: maxBlockGas},
		})
}

// buildCLOBProposalHandler constructs a CLOBMempool + CLOBTxSelector +
// PrepareProposalHandler wired together for integration testing.
func buildCLOBProposalHandler(clobBlockRatio float64, verifier baseapp.ProposalTxVerifier) (*CLOBMempool, sdk.PrepareProposalHandler) {
	mpool := NewCLOBMempool(100, testSignerExtractor{}, nil, testIsCLOBTx)

	noopDecoder := func([]byte) (sdk.Tx, error) { return nil, nil }
	noopValidate := func(sdk.Tx, []byte) error { return nil }
	selector := NewCLOBTxSelector(clobBlockRatio, testIsCLOBTx, noopValidate, noopDecoder)

	handler := baseapp.NewDefaultProposalHandler(mpool, verifier)
	handler.SetTxSelector(selector)
	// Use testSignerExtractor so *testTx instances are accepted without real signing infra.
	handler.SetSignerExtractionAdapter(testSignerExtractor{})

	return mpool, handler.PrepareProposalHandler()
}

const integrationMaxTxBytes = int64(1_000_000_000)

// TestCLOBIntegration_OrderCLOBFirst verifies that a CLOB tx inserted AFTER a
// regular tx still appears first in the proposal because CLOBMempool.SelectBy
// iterates clobPool before regularPool.
func TestCLOBIntegration_OrderCLOBFirst(t *testing.T) {
	sdkCtx := newIntegrationCtx(10_000_000)
	verifier := newMockProposalVerifier()
	mpool, prepareProposal := buildCLOBProposalHandler(0.3, verifier)

	regularSigner := sdk.AccAddress([]byte("integ-reg-signer____"))
	regularTx := newRegularTx(regularSigner, 1)
	clobTx := newCLOBTx(testSequencerAddr, 1)

	verifier.registerTx(regularTx, []byte("integ-regular-tx"))
	verifier.registerTx(clobTx, []byte("integ-clob-tx"))

	// Insert regular first; CLOB tx must still appear at the front of the proposal.
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, regularTx, 1_000_000))
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, clobTx, 1_000_000))

	resp, err := prepareProposal(sdkCtx, &abci.RequestPrepareProposal{
		MaxTxBytes: integrationMaxTxBytes,
	})
	require.NoError(t, err)
	require.Len(t, resp.Txs, 2)

	first, err := verifier.TxDecode(resp.Txs[0])
	require.NoError(t, err)
	require.True(t, testIsCLOBTx(first), "first proposal tx must be CLOB")

	second, err := verifier.TxDecode(resp.Txs[1])
	require.NoError(t, err)
	require.False(t, testIsCLOBTx(second), "second proposal tx must be regular")
}

// TestCLOBIntegration_CLOBGasExceedsQuota verifies that a CLOB tx whose gas
// exceeds the CLOB quota is excluded from the proposal while the regular tx
// is still included (rollover means the regular limit equals the full block gas).
func TestCLOBIntegration_CLOBGasExceedsQuota(t *testing.T) {
	// clobGasRatio=0.3 → clobGasLimit = 3 000 000
	// CLOB tx: 4M gas > 3M → skipped
	// Regular tx: 5M gas, effectiveRegularLimit = 10M - 0 = 10M → selected
	sdkCtx := newIntegrationCtx(10_000_000)
	verifier := newMockProposalVerifier()
	mpool, prepareProposal := buildCLOBProposalHandler(0.3, verifier)

	regularSigner := sdk.AccAddress([]byte("integ-quota-reg-sig_"))
	clobTx := newCLOBTx(testSequencerAddr, 1)
	regularTx := newRegularTx(regularSigner, 1)

	verifier.registerTx(clobTx, []byte("quota-clob-tx"))
	verifier.registerTx(regularTx, []byte("quota-reg-tx"))

	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, clobTx, 4_000_000))
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, regularTx, 5_000_000))

	resp, err := prepareProposal(sdkCtx, &abci.RequestPrepareProposal{
		MaxTxBytes: integrationMaxTxBytes,
	})
	require.NoError(t, err)
	require.Len(t, resp.Txs, 1, "oversized CLOB tx should be excluded; regular tx selected")

	only, err := verifier.TxDecode(resp.Txs[0])
	require.NoError(t, err)
	require.False(t, testIsCLOBTx(only), "only selected tx should be regular")
}

// TestCLOBIntegration_GasRollover verifies that when no CLOB txs are in the
// pool a regular tx whose gas exceeds the static CLOB-reserved portion (0.3 ×
// 10M = 3M) is still included because the full 10M rolls over.
func TestCLOBIntegration_GasRollover(t *testing.T) {
	// clobGasRatio=0.3 → clobGasLimit = 3M
	// No CLOB txs inserted.
	// Regular tx: 9M gas, effectiveRegularLimit = 10M - 0 = 10M → selected.
	// (Without rollover the limit would be 10M - 3M = 7M → rejected.)
	sdkCtx := newIntegrationCtx(10_000_000)
	verifier := newMockProposalVerifier()
	mpool, prepareProposal := buildCLOBProposalHandler(0.3, verifier)

	regularSigner := sdk.AccAddress([]byte("integ-rollover-sig__"))
	regularTx := newRegularTx(regularSigner, 1)
	verifier.registerTx(regularTx, []byte("rollover-reg-tx"))

	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, regularTx, 9_000_000))

	resp, err := prepareProposal(sdkCtx, &abci.RequestPrepareProposal{
		MaxTxBytes: integrationMaxTxBytes,
	})
	require.NoError(t, err)
	require.Len(t, resp.Txs, 1, "regular tx should be selected via unused CLOB gas rollover")

	only, err := verifier.TxDecode(resp.Txs[0])
	require.NoError(t, err)
	require.False(t, testIsCLOBTx(only))
}

// TestCLOBIntegration_PartialCLOBQuota verifies the mixed scenario: a CLOB tx
// consumes part of the CLOB quota, and the remainder plus unused quota rolls
// over so the regular tx fits within the effective regular limit.
func TestCLOBIntegration_PartialCLOBQuota(t *testing.T) {
	// clobGasRatio=0.3 → clobGasLimit = 3M
	// CLOB tx: 2M gas → clobGasUsed = 2M, selected
	// Regular tx: 7M gas, effectiveRegularLimit = 10M - 2M = 8M → 7M < 8M → selected
	sdkCtx := newIntegrationCtx(10_000_000)
	verifier := newMockProposalVerifier()
	mpool, prepareProposal := buildCLOBProposalHandler(0.3, verifier)

	regularSigner := sdk.AccAddress([]byte("integ-partial-reg___"))
	clobTx := newCLOBTx(testSequencerAddr, 1)
	regularTx := newRegularTx(regularSigner, 1)

	verifier.registerTx(clobTx, []byte("partial-clob-tx"))
	verifier.registerTx(regularTx, []byte("partial-reg-tx"))

	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, clobTx, 2_000_000))
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, regularTx, 7_000_000))

	resp, err := prepareProposal(sdkCtx, &abci.RequestPrepareProposal{
		MaxTxBytes: integrationMaxTxBytes,
	})
	require.NoError(t, err)
	require.Len(t, resp.Txs, 2, "both CLOB and regular tx should be selected")

	first, err := verifier.TxDecode(resp.Txs[0])
	require.NoError(t, err)
	require.True(t, testIsCLOBTx(first), "CLOB tx must appear first")

	second, err := verifier.TxDecode(resp.Txs[1])
	require.NoError(t, err)
	require.False(t, testIsCLOBTx(second), "regular tx must appear second")
}

// TestCLOBIntegration_UnlimitedGas verifies that when maxBlockGas=0 (unlimited)
// all txs are included regardless of their gas usage.
func TestCLOBIntegration_UnlimitedGas(t *testing.T) {
	sdkCtx := newIntegrationCtx(0) // 0 = unlimited
	verifier := newMockProposalVerifier()
	mpool, prepareProposal := buildCLOBProposalHandler(0.3, verifier)

	regularSigner := sdk.AccAddress([]byte("integ-unlim-reg-sig_"))
	clobTx := newCLOBTx(testSequencerAddr, 1)
	regularTx := newRegularTx(regularSigner, 1)

	verifier.registerTx(clobTx, []byte("unlim-clob-tx"))
	verifier.registerTx(regularTx, []byte("unlim-reg-tx"))

	// Both txs have enormous gas — should still be accepted with unlimited gas.
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, clobTx, 999_999_999))
	require.NoError(t, mpool.InsertWithGasWanted(sdkCtx, regularTx, 999_999_999))

	resp, err := prepareProposal(sdkCtx, &abci.RequestPrepareProposal{
		MaxTxBytes: integrationMaxTxBytes,
	})
	require.NoError(t, err)
	require.Len(t, resp.Txs, 2, "unlimited gas: both txs should be included")
}
