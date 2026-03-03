package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"filippo.io/age"
	abci "github.com/cometbft/cometbft/abci/types"
	cmttypes "github.com/cometbft/cometbft/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"

	"cosmossdk.io/core/address"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth/signing"
)

type BlockList struct {
	Addresses []string `mapstructure:"addresses"`
}

var _ baseapp.TxSelector = &ExtTxSelector{}

// ExtTxSelector extends a baseapp.TxSelector with extra tx validation method
type ExtTxSelector struct {
	baseapp.TxSelector
	TxDecoder  sdk.TxDecoder
	ValidateTx func(sdk.Tx, []byte) error
}

func NewExtTxSelector(parent baseapp.TxSelector, txDecoder sdk.TxDecoder, validateTx func(sdk.Tx, []byte) error) *ExtTxSelector {
	return &ExtTxSelector{
		TxSelector: parent,
		TxDecoder:  txDecoder,
		ValidateTx: validateTx,
	}
}

func (ts *ExtTxSelector) SelectTxForProposal(ctx context.Context, maxTxBytes, maxBlockGas uint64, memTx sdk.Tx, txBz []byte, gasWanted uint64) bool {
	if err := ts.ValidateTx(memTx, txBz); err != nil {
		return false
	}

	// don't pass `memTx` to parent selector so it don't check tx gas wanted against block gas limit,
	// it conflicts with the max-tx-gas-wanted logic.
	return ts.TxSelector.SelectTxForProposal(ctx, maxTxBytes, maxBlockGas, nil, txBz, gasWanted)
}

func (ts *ExtTxSelector) SelectTxForProposalFast(ctx context.Context, txs [][]byte) [][]byte {
	var invalidTxs []int
	for i, txBz := range txs {
		if err := ts.ValidateTx(nil, txBz); err != nil {
			invalidTxs = append(invalidTxs, i)
		}
	}

	if len(invalidTxs) > 0 {
		filtered := make([][]byte, 0, len(txs)-len(invalidTxs))
		var offset int
		for i, txBz := range txs {
			if offset < len(invalidTxs) && i == invalidTxs[offset] {
				offset++
				continue
			}
			filtered = append(filtered, txBz)
		}

		txs = filtered
	}

	return ts.TxSelector.SelectTxForProposalFast(ctx, txs)
}

// CLOBTxSelector is a TxSelector that reserves a configurable fraction of block
// resources (both gas and bytes) for CLOB (MsgSettleBatch) transactions. CLOB txs
// are expected to appear first in the SelectBy iteration order (via CLOBMempool).
// Unused CLOB quota rolls over to regular transactions so no block space is wasted.
type CLOBTxSelector struct {
	clobBlockRatio   float64
	clobGasLimit     uint64
	clobGasUsed      uint64
	maxBlockGas      uint64
	regularGasUsed   uint64
	clobBytesLimit   uint64
	clobBytesUsed    uint64
	regularBytesUsed uint64
	maxTxBytes       uint64
	selectedTxs      [][]byte
	initialized      bool
	isCLOBTx         func(sdk.Tx) bool
	validateTx       func(sdk.Tx, []byte) error
	txDecoder        sdk.TxDecoder
}

var _ baseapp.TxSelector = (*CLOBTxSelector)(nil)

// NewCLOBTxSelector creates a CLOBTxSelector.
func NewCLOBTxSelector(clobBlockRatio float64, isCLOBTx func(sdk.Tx) bool, validateTx func(sdk.Tx, []byte) error, txDecoder sdk.TxDecoder) *CLOBTxSelector {
	return &CLOBTxSelector{
		clobBlockRatio: clobBlockRatio,
		isCLOBTx:       isCLOBTx,
		validateTx:     validateTx,
		txDecoder:      txDecoder,
	}
}

func (ts *CLOBTxSelector) SelectedTxs(_ context.Context) [][]byte {
	txs := make([][]byte, len(ts.selectedTxs))
	copy(txs, ts.selectedTxs)
	return txs
}

func (ts *CLOBTxSelector) Clear() {
	ts.clobGasUsed = 0
	ts.regularGasUsed = 0
	ts.clobBytesUsed = 0
	ts.regularBytesUsed = 0
	ts.selectedTxs = ts.selectedTxs[:0]
	ts.initialized = false
}

func (ts *CLOBTxSelector) SelectTxForProposal(_ context.Context, maxTxBytes, maxBlockGas uint64, memTx sdk.Tx, txBz []byte, gasWanted uint64) bool {
	// Decode the tx if not provided, needed for validation and CLOB detection.
	if memTx == nil {
		var err error
		memTx, err = ts.txDecoder(txBz)
		if err != nil || memTx == nil {
			return false
		}
	}

	if err := ts.validateTx(memTx, txBz); err != nil {
		return false
	}

	// Lazy-init gas and byte limits from the first call.
	if !ts.initialized {
		ts.maxBlockGas = maxBlockGas
		ts.maxTxBytes = maxTxBytes
		if maxBlockGas > 0 {
			ts.clobGasLimit = uint64(ts.clobBlockRatio * float64(maxBlockGas))
		}
		ts.clobBytesLimit = uint64(ts.clobBlockRatio * float64(maxTxBytes))
		ts.initialized = true
	}

	txSize := uint64(cmttypes.ComputeProtoSizeForTxs([]cmttypes.Tx{txBz}))

	if ts.isCLOBTx(memTx) {
		// CLOB byte budget exceeded: skip this tx but keep iterating.
		if ts.clobBytesUsed+txSize > ts.clobBytesLimit {
			return false
		}
		// CLOB gas budget exceeded: skip this tx but keep iterating.
		if maxBlockGas > 0 && ts.clobGasUsed+gasWanted > ts.clobGasLimit {
			return false
		}
		ts.clobBytesUsed += txSize
		if maxBlockGas > 0 {
			ts.clobGasUsed += gasWanted
		}
	} else {
		// Rollover: regular txs may use any unused CLOB byte/gas quota.
		effectiveRegularBytesLimit := ts.maxTxBytes - ts.clobBytesUsed
		if ts.regularBytesUsed+txSize > effectiveRegularBytesLimit {
			return true
		}
		if maxBlockGas > 0 {
			effectiveRegularGasLimit := ts.maxBlockGas - ts.clobGasUsed
			if ts.regularGasUsed+gasWanted > effectiveRegularGasLimit {
				return true
			}
			ts.regularGasUsed += gasWanted
		}
		ts.regularBytesUsed += txSize
	}

	ts.selectedTxs = append(ts.selectedTxs, txBz)

	totalBytes := ts.clobBytesUsed + ts.regularBytesUsed
	totalGas := ts.clobGasUsed + ts.regularGasUsed
	return totalBytes >= ts.maxTxBytes || (maxBlockGas > 0 && totalGas >= maxBlockGas)
}

// SelectTxForProposalFast returns txs unchanged; only called for NoOpMempool path.
func (ts *CLOBTxSelector) SelectTxForProposalFast(_ context.Context, txs [][]byte) [][]byte {
	return txs
}

type ProposalHandler struct {
	TxDecoder sdk.TxDecoder
	// Identity is nil if it's not a validator node
	Identity      age.Identity
	blocklist     map[string]struct{}
	lastBlockList []byte
	addressCodec  address.Codec
}

func NewProposalHandler(txDecoder sdk.TxDecoder, identity age.Identity, addressCodec address.Codec) *ProposalHandler {
	return &ProposalHandler{
		TxDecoder:    txDecoder,
		Identity:     identity,
		blocklist:    make(map[string]struct{}),
		addressCodec: addressCodec,
	}
}

// SetBlockList don't fail if the identity is not set or the block list is empty.
func (h *ProposalHandler) SetBlockList(blob []byte) error {
	if h.Identity == nil {
		return nil
	}

	if bytes.Equal(h.lastBlockList, blob) {
		return nil
	}
	h.lastBlockList = make([]byte, len(blob))
	copy(h.lastBlockList, blob)

	if len(blob) == 0 {
		h.blocklist = make(map[string]struct{})
		return nil
	}

	reader, err := age.Decrypt(bytes.NewBuffer(blob), h.Identity)
	if err != nil {
		return err
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}

	var blocklist BlockList
	if err := json.Unmarshal(data, &blocklist); err != nil {
		return err
	}

	// convert to map
	m := make(map[string]struct{}, len(blocklist.Addresses))
	for _, s := range blocklist.Addresses {
		addr, err := h.addressCodec.StringToBytes(s)
		if err != nil {
			return fmt.Errorf("invalid bech32 address: %s, err: %w", s, err)
		}
		encoded, err := h.addressCodec.BytesToString(addr)
		if err != nil {
			return fmt.Errorf("invalid bech32 address: %s, err: %w", s, err)
		}
		m[encoded] = struct{}{}
	}

	h.blocklist = m
	return nil
}

func (h *ProposalHandler) ValidateTransaction(tx sdk.Tx, txBz []byte) error {
	if len(h.blocklist) == 0 {
		// fast path, accept all txs
		return nil
	}

	var err error
	if tx == nil {
		tx, err = h.TxDecoder(txBz)
		if err != nil {
			return err
		}
	}

	sigTx, ok := tx.(signing.SigVerifiableTx)
	if !ok {
		return fmt.Errorf("tx of type %T does not implement SigVerifiableTx", tx)
	}

	signers, err := sigTx.GetSigners()
	if err != nil {
		return err
	}
	for _, signer := range signers {
		encoded, err := h.addressCodec.BytesToString(signer)
		if err != nil {
			return fmt.Errorf("invalid bech32 address: %s, err: %w", signer, err)
		}
		if _, ok := h.blocklist[encoded]; ok {
			return fmt.Errorf("signer is blocked: %s", encoded)
		}
	}

	for _, msg := range tx.GetMsgs() {
		msgEthTx, ok := msg.(*evmtypes.MsgEthereumTx)
		if ok {
			ethTx := msgEthTx.AsTransaction()
			// check the destination address
			if ethTx.To() != nil {
				encoded, err := h.addressCodec.BytesToString(ethTx.To().Bytes())
				if err != nil {
					return fmt.Errorf("invalid bech32 address: %s, err: %w", ethTx.To(), err)
				}
				if _, ok := h.blocklist[encoded]; ok {
					return fmt.Errorf("destination address is blocked: %s", encoded)
				}
			}
			// check EIP-7702 authorisation list
			if ethTx.SetCodeAuthorizations() != nil {
				for _, auth := range ethTx.SetCodeAuthorizations() {
					addr, err := auth.Authority()
					if err == nil {
						if _, ok := h.blocklist[sdk.AccAddress(addr.Bytes()).String()]; ok {
							return fmt.Errorf("signer is blocked: %s", addr.String())
						}
					}
					// check the target address
					encoded, err := h.addressCodec.BytesToString(auth.Address.Bytes())
					if err != nil {
						return fmt.Errorf("invalid bech32 address: %s, err: %w", auth.Address, err)
					}
					if _, ok := h.blocklist[encoded]; ok {
						return fmt.Errorf("authorisation address is blocked: %s", encoded)
					}
				}
			}
		}
	}

	return nil
}

func (h *ProposalHandler) ProcessProposalHandler() sdk.ProcessProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		if len(h.blocklist) == 0 {
			// fast path, accept all txs
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
		}

		for _, txBz := range req.Txs {
			if err := h.ValidateTransaction(nil, txBz); err != nil {
				return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
			}
		}

		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	}
}

// noneIdentity is a dummy identity which postpone the failure to the decryption time
type noneIdentity struct{}

var _ age.Identity = noneIdentity{}

func (noneIdentity) Unwrap([]*age.Stanza) ([]byte, error) {
	return nil, age.ErrIncorrectIdentity
}
