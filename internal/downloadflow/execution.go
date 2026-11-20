package downloadflow

import (
	"time"
)

// IntentExecutionOutcome describes the result needed by a serial automatic
// subscription candidate loop. It deliberately does not expose River actions.
type IntentExecutionOutcome string

const (
	// IntentExecutionAccepted means the downloader accepted or reconciled the intent.
	IntentExecutionAccepted IntentExecutionOutcome = "accepted"
	// IntentExecutionDefinitiveFailureRecovered means the candidate failed and
	// its subscription ownership was atomically released.
	IntentExecutionDefinitiveFailureRecovered IntentExecutionOutcome = "definitive_failure_recovered"
	// IntentExecutionPendingReconciliation means the external result is unknown;
	// callers must keep the current intent and must not try another candidate.
	IntentExecutionPendingReconciliation IntentExecutionOutcome = "pending_reconciliation"
	// IntentExecutionRetryableFailure means execution could not safely reach a
	// candidate terminal result.
	IntentExecutionRetryableFailure IntentExecutionOutcome = "retryable_failure"
	// IntentExecutionPermanentCandidateFailure means persisted candidate facts
	// are unusable and no external side effect was observed.
	IntentExecutionPermanentCandidateFailure IntentExecutionOutcome = "permanent_candidate_failure"
)

// IntentExecutionResult is the synchronous domain result consumed by RSS and
// precise-search candidate loops.
type IntentExecutionResult struct {
	Outcome    IntentExecutionOutcome
	RetryAfter time.Duration
}
