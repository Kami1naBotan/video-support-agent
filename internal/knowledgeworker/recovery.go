package knowledgeworker

import "context"

type RecoveryStore interface {
	RecordBuildFailure(context.Context, BuildClaim, string) error
	AdoptBlockedFailure(context.Context) (bool, error)
	RecoverPending(context.Context) (int, error)
}
