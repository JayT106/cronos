package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	protov2 "google.golang.org/protobuf/proto"

	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"
	exchangetypes "github.com/crypto-org-chain/cronos/x/exchange/types"
)

// testTx is a minimal sdk.Tx implementation for testing CLOBMempool routing.
type testTx struct {
	msgs   []sdk.Msg
	signer sdk.AccAddress
	nonce  uint64
}

func (tx *testTx) GetMsgs() []sdk.Msg                    { return tx.msgs }
func (tx *testTx) GetMsgsV2() ([]protov2.Message, error) { return nil, nil }

// testSignerExtractor is a SignerExtractionAdapter that reads signer/nonce from testTx.
type testSignerExtractor struct{}

func (testSignerExtractor) GetSigners(tx sdk.Tx) ([]mempool.SignerData, error) {
	t := tx.(*testTx)
	return []mempool.SignerData{
		mempool.NewSignerData(t.signer, t.nonce),
	}, nil
}

// newTestSDKCtx returns an sdk.Context suitable for mempool operations.
// The PriorityNonceMempool's default TxPriority requires an sdk.Context.
func newTestSDKCtx() sdk.Context {
	return sdk.NewContext(nil, cmtproto.Header{}, false, nil)
}

func newTestCLOBMempool() *CLOBMempool {
	return NewCLOBMempool(100, testSignerExtractor{}, nil)
}

// newCLOBTx creates a testTx containing a MsgSettleBatch (CLOB tx).
func newCLOBTx(signer sdk.AccAddress, nonce uint64) *testTx {
	return &testTx{
		msgs:   []sdk.Msg{&exchangetypes.MsgSettleBatch{}},
		signer: signer,
		nonce:  nonce,
	}
}

// newRegularTx creates a testTx with no CLOB messages.
func newRegularTx(signer sdk.AccAddress, nonce uint64) *testTx {
	return &testTx{
		msgs:   []sdk.Msg{},
		signer: signer,
		nonce:  nonce,
	}
}

func TestCLOBMempoolRouting(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	require.NoError(t, pool.Insert(ctx, clobTx))
	require.NoError(t, pool.Insert(ctx, regularTx))

	require.Equal(t, 1, pool.clobPool.CountTx())
	require.Equal(t, 1, pool.regularPool.CountTx())
	require.Equal(t, 2, pool.CountTx())
}

func TestCLOBMempoolInsertWithGasWanted(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	require.NoError(t, pool.InsertWithGasWanted(ctx, clobTx, 1000))
	require.NoError(t, pool.InsertWithGasWanted(ctx, regularTx, 2000))

	require.Equal(t, 1, pool.clobPool.CountTx())
	require.Equal(t, 1, pool.regularPool.CountTx())
}

func TestCLOBMempoolRemove(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	require.NoError(t, pool.Insert(ctx, clobTx))
	require.NoError(t, pool.Insert(ctx, regularTx))
	require.Equal(t, 2, pool.CountTx())

	// Remove the CLOB tx — should succeed and leave regularPool intact.
	require.NoError(t, pool.Remove(clobTx))
	require.Equal(t, 0, pool.clobPool.CountTx())
	require.Equal(t, 1, pool.regularPool.CountTx())

	// Remove the regular tx.
	require.NoError(t, pool.Remove(regularTx))
	require.Equal(t, 0, pool.CountTx())
}

func TestCLOBMempoolSelectByOrder(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	// Insert regular first, then CLOB — CLOB should still come out first.
	require.NoError(t, pool.Insert(ctx, regularTx))
	require.NoError(t, pool.Insert(ctx, clobTx))

	var order []bool // true = CLOB, false = regular
	pool.SelectBy(ctx, nil, func(tx mempool.Tx) bool {
		order = append(order, isCLOBTx(tx.Tx))
		return true
	})

	require.Len(t, order, 2)
	require.True(t, order[0], "first selected tx should be CLOB")
	require.False(t, order[1], "second selected tx should be regular")
}

func TestCLOBMempoolSelectByHalt(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	require.NoError(t, pool.Insert(ctx, clobTx))
	require.NoError(t, pool.Insert(ctx, regularTx))

	var count int
	// Return false after first tx to halt iteration.
	pool.SelectBy(ctx, nil, func(tx mempool.Tx) bool {
		count++
		return false
	})

	// Only CLOB tx should have been visited.
	require.Equal(t, 1, count)
}

func TestCLOBMempoolSelectIterator(t *testing.T) {
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	clobTx := newCLOBTx(signer, 1)
	regularTx := newRegularTx(signer, 2)

	require.NoError(t, pool.Insert(ctx, clobTx))
	require.NoError(t, pool.Insert(ctx, regularTx))

	var txs []sdk.Tx
	for iter := pool.Select(ctx, nil); iter != nil; iter = iter.Next() {
		txs = append(txs, iter.Tx().Tx)
	}

	require.Len(t, txs, 2)
	require.True(t, isCLOBTx(txs[0]), "first iterated tx should be CLOB")
	require.False(t, isCLOBTx(txs[1]), "second iterated tx should be regular")
}

func TestCLOBMempoolRemoveNotFound(t *testing.T) {
	pool := newTestCLOBMempool()
	signer := sdk.AccAddress([]byte("signer__________"))

	tx := newRegularTx(signer, 1)
	err := pool.Remove(tx)
	require.ErrorIs(t, err, mempool.ErrTxNotFound)
}

func TestCLOBMempoolMaxTxCapacity(t *testing.T) {
	// MaxTx=1 means each sub-pool accepts only one transaction.
	pool := NewCLOBMempool(1, testSignerExtractor{}, nil)
	ctx := newTestSDKCtx()
	signer1 := sdk.AccAddress([]byte("signer-max-1________"))
	signer2 := sdk.AccAddress([]byte("signer-max-2________"))

	require.NoError(t, pool.Insert(ctx, newCLOBTx(signer1, 1)))
	// Second CLOB tx from a different sender exceeds MaxTx=1 for clobPool.
	err := pool.Insert(ctx, newCLOBTx(signer2, 1))
	require.ErrorIs(t, err, mempool.ErrMempoolTxMaxCapacity)

	require.NoError(t, pool.Insert(ctx, newRegularTx(signer1, 1)))
	// Second regular tx from a different sender exceeds MaxTx=1 for regularPool.
	err = pool.Insert(ctx, newRegularTx(signer2, 1))
	require.ErrorIs(t, err, mempool.ErrMempoolTxMaxCapacity)
}

func TestCLOBMempoolEmptyCLOBPool(t *testing.T) {
	// When clobPool is empty, SelectBy should iterate only the regular pool.
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()
	signer := sdk.AccAddress([]byte("signer__________"))

	require.NoError(t, pool.Insert(ctx, newRegularTx(signer, 1)))

	var visited []sdk.Tx
	pool.SelectBy(ctx, nil, func(tx mempool.Tx) bool {
		visited = append(visited, tx.Tx)
		return true
	})

	require.Len(t, visited, 1)
	require.False(t, isCLOBTx(visited[0]), "only regular tx should be visited")
}

func TestCLOBMempoolMultipleCLOBSigners(t *testing.T) {
	// Multiple CLOB txs from different signers must all land in clobPool.
	pool := newTestCLOBMempool()
	ctx := newTestSDKCtx()

	signers := []sdk.AccAddress{
		sdk.AccAddress([]byte("clob-multi-sig-1____")),
		sdk.AccAddress([]byte("clob-multi-sig-2____")),
		sdk.AccAddress([]byte("clob-multi-sig-3____")),
	}
	for _, s := range signers {
		require.NoError(t, pool.Insert(ctx, newCLOBTx(s, 1)))
	}

	require.Equal(t, len(signers), pool.clobPool.CountTx())
	require.Equal(t, 0, pool.regularPool.CountTx())
	require.Equal(t, len(signers), pool.CountTx())
}
