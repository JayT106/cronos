package config

import sdk "github.com/cosmos/cosmos-sdk/types"

// SetBech32Prefixes sets the global prefixes to be used when serializing addresses and public keys to Bech32 strings.
func SetBech32Prefixes(config *sdk.Config) {
	config.SetBech32PrefixForAccount(Bech32PrefixAccAddr, Bech32PrefixAccPub)
	config.SetBech32PrefixForValidator(Bech32PrefixValAddr, Bech32PrefixValPub)
	config.SetBech32PrefixForConsensusNode(Bech32PrefixConsAddr, Bech32PrefixConsPub)
}

type CronosConfig struct {
	// Set to true to disable tx replacement.
	DisableTxReplacement bool `mapstructure:"disable-tx-replacement"`
	// Set to true to disable optimistic execution.
	DisableOptimisticExecution bool `mapstructure:"disable-optimistic-execution"`
	// CLOBGasRatio is the fraction of block gas reserved for CLOB (MsgSettleBatch) transactions.
	// CLOB txs go first in every block, and unused quota rolls over to regular txs.
	// Range: [0.0, 1.0]. Set to 0.0 to disable.
	CLOBGasRatio float64 `mapstructure:"clob-gas-ratio"`
}

func DefaultCronosConfig() CronosConfig {
	return CronosConfig{
		DisableTxReplacement:       false,
		DisableOptimisticExecution: false,
		CLOBGasRatio:               0.0,
	}
}
