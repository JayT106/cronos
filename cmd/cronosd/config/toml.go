package config

// DefaultCronosConfigTemplate defines the configuration template for cronos configuration
const DefaultCronosConfigTemplate = `
###############################################################################
###                             Cronos Configuration                       ###
###############################################################################

[cronos]

# Set to true to disable tx replacement.
disable-tx-replacement = {{ .Cronos.DisableTxReplacement }}

# Set to true to disable optimistic execution (not recommended on validator nodes).
disable-optimistic-execution = {{ .Cronos.DisableOptimisticExecution }}

# Fraction of block resources (gas and bytes) reserved for CLOB (MsgSettleBatch) transactions [0.0 to 1.0].
# CLOB transactions are placed first in every block.
# Unused CLOB quota rolls over to regular EVM transactions.
# Set to 0.0 to disable (default).
clob-block-ratio = {{ .Cronos.CLOBBlockRatio }}
`
