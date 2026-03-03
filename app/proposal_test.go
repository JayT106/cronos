package app

import (
	"context"
	"errors"
	"testing"

	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type mockTxSelector struct {
	baseapp.TxSelector
}

func (mts *mockTxSelector) SelectTxForProposalFast(ctx context.Context, txs [][]byte) [][]byte {
	// For testing purposes, simply return the txs as is
	return txs
}

func TestSelectTxForProposalFast(t *testing.T) {
	ctx := context.Background()

	txDecoder := func(txBytes []byte) (sdk.Tx, error) {
		// Mock tx decoder; returns a dummy tx
		return nil, nil
	}

	validateTx := func(tx sdk.Tx, txBz []byte) error {
		// Mock validation logic: return error if txBz is "invalid"
		if string(txBz) == "invalid" {
			return errors.New("invalid tx")
		}
		return nil
	}

	mockSelector := &mockTxSelector{}

	extTxSelector := NewExtTxSelector(mockSelector, txDecoder, validateTx)

	t.Run("Empty transaction list", func(t *testing.T) {
		txs := [][]byte{}
		result := extTxSelector.SelectTxForProposalFast(ctx, txs)
		require.Empty(t, result)
	})

	t.Run("All valid transactions", func(t *testing.T) {
		txs := [][]byte{[]byte("valid1"), []byte("valid2"), []byte("valid3")}
		result := extTxSelector.SelectTxForProposalFast(ctx, txs)
		require.Equal(t, txs, result)
	})

	t.Run("All invalid transactions", func(t *testing.T) {
		txs := [][]byte{[]byte("invalid"), []byte("invalid"), []byte("invalid")}
		result := extTxSelector.SelectTxForProposalFast(ctx, txs)
		require.Empty(t, result)
	})

	t.Run("Mixed valid and invalid transactions", func(t *testing.T) {
		txs := [][]byte{[]byte("valid1"), []byte("invalid"), []byte("valid2"), []byte("invalid"), []byte("valid3")}
		expected := [][]byte{[]byte("valid1"), []byte("valid2"), []byte("valid3")}
		result := extTxSelector.SelectTxForProposalFast(ctx, txs)
		require.Equal(t, expected, result)
	})

	t.Run("Edge cases in the filtering logic", func(t *testing.T) {
		// Edge case: first and last transactions are invalid
		txs := [][]byte{[]byte("invalid"), []byte("valid1"), []byte("valid2"), []byte("invalid")}
		expected := [][]byte{[]byte("valid1"), []byte("valid2")}
		result := extTxSelector.SelectTxForProposalFast(ctx, txs)
		require.Equal(t, expected, result)
	})
}

// --- CLOBTxSelector tests ---

func newTestCLOBTxSelector(ratio float64, isCLOBFn func(sdk.Tx) bool) *CLOBTxSelector {
	validateFn := func(sdk.Tx, []byte) error { return nil }
	decoderFn := func([]byte) (sdk.Tx, error) { return nil, nil }
	return NewCLOBTxSelector(ratio, isCLOBFn, validateFn, decoderFn)
}

// makeSelectorTx returns a test tx identified by the isClob flag.
// CLOB txs use testSequencerAddr; regular txs use a different signer.
func makeSelectorTx(isClob bool) sdk.Tx {
	if isClob {
		return newCLOBTx(testSequencerAddr, 1)
	}
	signer := sdk.AccAddress([]byte("proposaltestsigner__"))
	return newRegularTx(signer, 1)
}

func TestCLOBTxSelectorCLOBGasLimit(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000 // high enough not to be the limit
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx) // 3M CLOB gas limit
	ctx := context.Background()
	txBz := []byte("tx")
	clobTx := makeSelectorTx(true)

	// First CLOB tx: 2M gas → within 3M limit, accepted.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, clobTx, txBz, 2_000_000)
	require.False(t, stop)
	require.Len(t, sel.SelectedTxs(ctx), 1)

	// Second CLOB tx: another 2M gas → total 4M > 3M limit, skipped but not halted.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, clobTx, txBz, 2_000_000)
	require.False(t, stop, "should skip (not halt) when CLOB gas limit exceeded")
	require.Len(t, sel.SelectedTxs(ctx), 1, "second CLOB tx should not be selected")
}

func TestCLOBTxSelectorRollover(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")
	regularTx := makeSelectorTx(false)

	// No CLOB txs used; effective regular limit = 10M - 0 = 10M (full rollover).
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, regularTx, txBz, 9_000_000)
	require.False(t, stop)
	require.Len(t, sel.SelectedTxs(ctx), 1)

	// Second regular tx: 1M gas → total 10M, exactly at limit.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, regularTx, txBz, 1_000_000)
	require.True(t, stop, "should halt when block gas is exactly exhausted")
	require.Len(t, sel.SelectedTxs(ctx), 2, "second regular tx should be selected")
}

func TestCLOBTxSelectorMixed(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx) // 3M CLOB, 7M available for regular (with rollover)
	ctx := context.Background()
	txBz := []byte("tx")
	clobTx := makeSelectorTx(true)
	regularTx := makeSelectorTx(false)

	// Use 2M CLOB gas.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, clobTx, txBz, 2_000_000)
	require.False(t, stop)

	// Regular tx: effectiveRegularLimit = 10M - 2M = 8M.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, regularTx, txBz, 8_000_000)
	require.True(t, stop, "should halt: total gas = 10M")
	require.Len(t, sel.SelectedTxs(ctx), 2)
}

func TestCLOBTxSelectorRegularGasExceeded(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")
	clobTx := makeSelectorTx(true)
	regularTx := makeSelectorTx(false)

	// Use 2M CLOB gas → effectiveRegularLimit = 8M.
	sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, clobTx, txBz, 2_000_000)

	// Regular tx needing 9M > 8M limit → should halt.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, regularTx, txBz, 9_000_000)
	require.True(t, stop, "should halt when regular tx exceeds effective limit")
	require.Len(t, sel.SelectedTxs(ctx), 1, "oversized regular tx should not be selected")
}

func TestCLOBTxSelectorUnlimitedGas(t *testing.T) {
	const (
		maxBlockGas = 0 // unlimited
		maxTxBytes  = 1_000_000_000
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")

	// When maxBlockGas=0, gas checks are skipped entirely.
	for i := 0; i < 5; i++ {
		stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, makeSelectorTx(i%2 == 0), txBz, 999_999_999)
		require.False(t, stop)
	}
	require.Len(t, sel.SelectedTxs(ctx), 5)
}

func TestCLOBTxSelectorValidationError(t *testing.T) {
	validateFn := func(_ sdk.Tx, txBz []byte) error {
		if string(txBz) == "invalid" {
			return errors.New("blocked")
		}
		return nil
	}
	decoderFn := func([]byte) (sdk.Tx, error) { return nil, nil }
	sel := NewCLOBTxSelector(0.3, testIsCLOBTx, validateFn, decoderFn)
	ctx := context.Background()

	clobTx := makeSelectorTx(true)
	stop := sel.SelectTxForProposal(ctx, 1_000_000_000, 10_000_000, clobTx, []byte("invalid"), 1_000)
	require.False(t, stop, "invalid tx should be skipped (not halt)")
	require.Empty(t, sel.SelectedTxs(ctx))
}

func TestCLOBTxSelectorClear(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000
	)

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")

	sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, makeSelectorTx(true), txBz, 1_000_000)
	require.Len(t, sel.SelectedTxs(ctx), 1)

	sel.Clear()
	require.Empty(t, sel.SelectedTxs(ctx))
	require.False(t, sel.initialized)
	require.Equal(t, uint64(0), sel.clobGasUsed)
	require.Equal(t, uint64(0), sel.regularGasUsed)
	require.Equal(t, uint64(0), sel.clobBytesUsed)
	require.Equal(t, uint64(0), sel.regularBytesUsed)
}

func TestCLOBTxSelectorBytesLimit(t *testing.T) {
	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()

	// maxTxBytes is tiny (1 byte). ComputeProtoSizeForTxs always returns > 1
	// for any non-empty txBz, so the selector must halt immediately.
	stop := sel.SelectTxForProposal(ctx, 1, 10_000_000, makeSelectorTx(false), []byte("tx"), 1_000)
	require.True(t, stop, "should halt when tx exceeds the byte budget")
	require.Empty(t, sel.SelectedTxs(ctx), "oversized tx must not be selected")
}

func TestCLOBTxSelectorCLOBRatioFull(t *testing.T) {
	const (
		maxBlockGas = 10_000_000
		maxTxBytes  = 1_000_000_000
	)

	// ratio=1.0 → entire block reserved for CLOB; regular txs get zero budget
	// unless CLOB quota is unused (rollover).
	sel := newTestCLOBTxSelector(1.0, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")

	// CLOB tx fits in the full quota.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, makeSelectorTx(true), txBz, 5_000_000)
	require.False(t, stop)
	require.Len(t, sel.SelectedTxs(ctx), 1)

	// Regular tx: effectiveRegularLimit = 10M - 5M = 5M; 6M > 5M → halt.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, makeSelectorTx(false), txBz, 6_000_000)
	require.True(t, stop, "regular tx exceeding rollover budget should halt")
	require.Len(t, sel.SelectedTxs(ctx), 1, "regular tx must not be selected")
}

func TestCLOBTxSelectorClearAndReuse(t *testing.T) {
	const maxTxBytes = 1_000_000_000

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	txBz := []byte("tx")

	// First block: maxBlockGas=10M, clobGasLimit=3M.
	sel.SelectTxForProposal(ctx, maxTxBytes, 10_000_000, makeSelectorTx(true), txBz, 2_000_000)
	require.Len(t, sel.SelectedTxs(ctx), 1)
	require.Equal(t, uint64(2_000_000), sel.clobGasUsed)

	// Clear simulates the end-of-block reset.
	sel.Clear()
	require.False(t, sel.initialized)

	// Second block: maxBlockGas=20M — clobGasLimit should be re-computed to 6M.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, 20_000_000, makeSelectorTx(true), txBz, 5_000_000)
	require.False(t, stop)
	require.Equal(t, uint64(20_000_000), sel.maxBlockGas, "maxBlockGas should refresh after Clear")
	require.Equal(t, uint64(6_000_000), sel.clobGasLimit, "clobGasLimit should be 0.3 × 20M")
	require.Equal(t, uint64(300_000_000), sel.clobBytesLimit, "clobBytesLimit should be 0.3 × 1B")
	require.Len(t, sel.SelectedTxs(ctx), 1)
}

func TestCLOBTxSelectorCLOBBytesLimit(t *testing.T) {
	// Set maxTxBytes so clobBytesLimit < txSize but the overall budget can fit the tx.
	txBz := []byte("tx")
	txSize := uint64(cmttypes.ComputeProtoSizeForTxs([]cmttypes.Tx{txBz}))
	maxTxBytes := txSize * 2 // clobBytesLimit = 0.3 * 2*txSize ≈ 0.6*txSize < txSize

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	clobTx := makeSelectorTx(true)
	regularTx := makeSelectorTx(false)

	// CLOB tx: txSize > clobBytesLimit → skipped (not halted).
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, 0, clobTx, txBz, 0)
	require.False(t, stop, "should skip (not halt) when CLOB tx exceeds byte quota")
	require.Empty(t, sel.SelectedTxs(ctx))

	// Regular tx: effectiveRegularBytesLimit = maxTxBytes - 0 = maxTxBytes → fits.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, 0, regularTx, txBz, 0)
	require.False(t, stop)
	require.Len(t, sel.SelectedTxs(ctx), 1, "regular tx should be accepted despite CLOB byte limit")
}

func TestCLOBTxSelectorBytesRollover(t *testing.T) {
	// When no CLOB txs are present, regular txs get the full byte budget.
	txBz := []byte("tx")
	txSize := uint64(cmttypes.ComputeProtoSizeForTxs([]cmttypes.Tx{txBz}))
	maxTxBytes := txSize * 3 // room for 3 txs total

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	regularTx := makeSelectorTx(false)

	// No CLOB txs. effectiveRegularBytesLimit = maxTxBytes - 0 = maxTxBytes.
	// First two regular txs should fit.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, 0, regularTx, txBz, 0)
	require.False(t, stop)
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, 0, regularTx, txBz, 0)
	require.False(t, stop)

	// Third regular tx fills the budget exactly.
	stop = sel.SelectTxForProposal(ctx, maxTxBytes, 0, regularTx, txBz, 0)
	require.True(t, stop, "should halt: total bytes == maxTxBytes")
	require.Len(t, sel.SelectedTxs(ctx), 3)
}

func TestCLOBTxSelectorMixedBytesAndGas(t *testing.T) {
	// Verify that a CLOB tx can be rejected by bytes even when gas is available.
	txBz := []byte("tx")
	txSize := uint64(cmttypes.ComputeProtoSizeForTxs([]cmttypes.Tx{txBz}))
	// clobBytesLimit = 0.3 * 2*txSize < txSize → byte-limited
	maxTxBytes := txSize * 2

	sel := newTestCLOBTxSelector(0.3, testIsCLOBTx)
	ctx := context.Background()
	clobTx := makeSelectorTx(true)

	// CLOB tx has plenty of gas budget (10M * 0.3 = 3M > 1000) but not enough bytes.
	stop := sel.SelectTxForProposal(ctx, maxTxBytes, 10_000_000, clobTx, txBz, 1_000)
	require.False(t, stop, "CLOB tx should be skipped due to byte limit, not gas")
	require.Empty(t, sel.SelectedTxs(ctx))
}
