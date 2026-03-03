package app

import (
	"bytes"
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

// NewCLOBTxDetector returns a function that classifies a transaction as CLOB
// if it is a MsgEthereumTx sent by the given sequencer address.
// If sequencerAddr is empty, the returned function always returns false.
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

// CLOBMempool is a mempool that separates CLOB transactions from regular
// transactions. CLOB txs are iterated first in SelectBy/Select, giving them
// priority placement at the front of every block proposal.
type CLOBMempool struct {
	clobPool    *mempool.PriorityNonceMempool[int64]
	regularPool *mempool.PriorityNonceMempool[int64]
	isCLOBTx    func(sdk.Tx) bool
}

var _ mempool.ExtMempool = (*CLOBMempool)(nil)

// NewCLOBMempool creates a CLOBMempool backed by two PriorityNonceMempools
// sharing the same SignerExtractionAdapter and TxReplacement policy.
func NewCLOBMempool(maxTx int, signerExtractor mempool.SignerExtractionAdapter, txReplacement func(int64, int64, sdk.Tx, sdk.Tx) bool, isCLOBTx func(sdk.Tx) bool) *CLOBMempool {
	cfg := mempool.PriorityNonceMempoolConfig[int64]{
		TxPriority:      mempool.NewDefaultTxPriority(),
		SignerExtractor: signerExtractor,
		MaxTx:           maxTx,
		TxReplacement:   txReplacement,
	}
	return &CLOBMempool{
		clobPool:    mempool.NewPriorityMempool(cfg),
		regularPool: mempool.NewPriorityMempool(cfg),
		isCLOBTx:    isCLOBTx,
	}
}

// Insert routes the transaction to clobPool if the detector identifies it as
// CLOB, otherwise to regularPool.
func (cm *CLOBMempool) Insert(ctx context.Context, tx sdk.Tx) error {
	if cm.isCLOBTx(tx) {
		return cm.clobPool.Insert(ctx, tx)
	}
	return cm.regularPool.Insert(ctx, tx)
}

// InsertWithGasWanted routes the transaction to clobPool if the detector
// identifies it as CLOB, otherwise to regularPool.
func (cm *CLOBMempool) InsertWithGasWanted(ctx context.Context, tx sdk.Tx, gasWanted uint64) error {
	if cm.isCLOBTx(tx) {
		return cm.clobPool.InsertWithGasWanted(ctx, tx, gasWanted)
	}
	return cm.regularPool.InsertWithGasWanted(ctx, tx, gasWanted)
}

// Remove attempts to remove the transaction from clobPool first, then regularPool.
func (cm *CLOBMempool) Remove(tx sdk.Tx) error {
	err := cm.clobPool.Remove(tx)
	if err == nil {
		return nil
	}
	if err != mempool.ErrTxNotFound {
		return err
	}
	return cm.regularPool.Remove(tx)
}

// CountTx returns the total number of transactions across both sub-pools.
func (cm *CLOBMempool) CountTx() int {
	return cm.clobPool.CountTx() + cm.regularPool.CountTx()
}

// chainedIterator chains two mempool iterators, exhausting the first before the second.
type chainedIterator struct {
	first  mempool.Iterator
	second mempool.Iterator
}

// newChainedIterator returns a combined iterator over first then second.
// If first is nil, second is returned directly.
func newChainedIterator(first, second mempool.Iterator) mempool.Iterator {
	if first != nil {
		return &chainedIterator{first: first, second: second}
	}
	return second
}

func (i *chainedIterator) Next() mempool.Iterator {
	next := i.first.Next()
	if next != nil {
		return &chainedIterator{first: next, second: i.second}
	}
	return i.second
}

func (i *chainedIterator) Tx() mempool.Tx {
	return i.first.Tx()
}

// Select returns an iterator over CLOB txs first, then regular txs.
func (cm *CLOBMempool) Select(ctx context.Context, txs [][]byte) mempool.Iterator {
	clobIter := cm.clobPool.Select(ctx, txs)
	regularIter := cm.regularPool.Select(ctx, txs)
	return newChainedIterator(clobIter, regularIter)
}

// SelectBy iterates CLOB txs via callback first, then regular txs (if the callback
// continues returning true after the CLOB phase).
func (cm *CLOBMempool) SelectBy(ctx context.Context, txs [][]byte, callback func(mempool.Tx) bool) {
	cont := true
	cm.clobPool.SelectBy(ctx, txs, func(tx mempool.Tx) bool {
		cont = callback(tx)
		return cont
	})
	if cont {
		cm.regularPool.SelectBy(ctx, txs, callback)
	}
}
