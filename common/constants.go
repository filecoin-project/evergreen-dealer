package common

import (
	filaddr "github.com/filecoin-project/go-address"
	filactor "github.com/filecoin-project/specs-actors/actors/builtin"
)

const (
	AppName      = "evergreen-dealer"
	PromInstance = "dataprogs_evergreen"

	FilGenesisUnix      = 1598306400
	FilDefaultLookback  = 10
	ApiMaxTipsetsBehind = 3 // keep in mind that a nul tipset is indistinguishable from loss of sync - do not set too low

	MaxOutstandingGiB              = int64(1024)
	ProposalStartDelayFromMidnight = (72 + 16) * filactor.EpochsInHour
	ProposalDuration               = 532 * filactor.EpochsInDay
)

var EgWallet, _ = filaddr.NewFromString("f01787692")