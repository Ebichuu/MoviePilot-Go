package downloadflow

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

func TestHandlerExecuteIntentReturnsSerialCandidateOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*handlerFixture)
		want      IntentExecutionOutcome
		wantRetry time.Duration
		wantErr   error
	}{
		{
			name: "downloader accepted",
			want: IntentExecutionAccepted,
		},
		{
			name: "definitive failure recovered",
			configure: func(fixture *handlerFixture) {
				fixture.submitter.addErr = downloader.ErrDefinitiveAddFailure
			},
			want: IntentExecutionDefinitiveFailureRecovered,
		},
		{
			name: "unknown add result remains pending reconciliation",
			configure: func(fixture *handlerFixture) {
				fixture.submitter.addErr = downloader.ErrReconciliationPending
			},
			want:      IntentExecutionPendingReconciliation,
			wantRetry: reconciliationSnooze,
		},
		{
			name: "temporary source failure is retryable",
			configure: func(fixture *handlerFixture) {
				fixture.resolver.err = errors.New("temporary source failure with passkey=secret")
			},
			want:    IntentExecutionRetryableFailure,
			wantErr: ErrDownloadSourceUnavailable,
		},
		{
			name: "invalid persisted plan is a permanent candidate failure",
			configure: func(fixture *handlerFixture) {
				fixture.store.planErr = database.ErrDownloadIntentPlanNotFound
			},
			want: IntentExecutionPermanentCandidateFailure,
		},
		{
			name: "permanent failure recovery uncertainty stops the candidate chain",
			configure: func(fixture *handlerFixture) {
				fixture.store.planErr = database.ErrDownloadIntentPlanNotFound
				fixture.store.recoveryErr = errors.New("postgres recovery failed with password=secret")
			},
			want:    IntentExecutionRetryableFailure,
			wantErr: ErrDownloadStateUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newHandlerFixture(t, downloader.IntentPlanned, "resource-serial")
			if tt.configure != nil {
				tt.configure(fixture)
			}
			got, err := fixture.handler.ExecuteIntent(t.Context(), fixture.args.Username, fixture.args.IntentID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ExecuteIntent() error = %v, want %v", err, tt.wantErr)
			}
			if got.Outcome != tt.want || got.RetryAfter != tt.wantRetry {
				t.Fatalf("ExecuteIntent() = %#v, want outcome=%q retry=%s", got, tt.want, tt.wantRetry)
			}
			if tt.want == IntentExecutionPermanentCandidateFailure {
				calls := fixture.store.recoverySnapshot()
				if len(calls) != 1 || len(calls[0].fromStates) != 1 ||
					calls[0].fromStates[0] != downloader.IntentPlanned {
					t.Fatalf("permanent failure recovery calls = %#v", calls)
				}
			}
			if err != nil && (errors.Is(err, fixture.resolver.err) || containsSensitiveExecutionError(err)) {
				t.Fatalf("ExecuteIntent() leaked dependency error: %v", err)
			}
		})
	}
}

func TestHandlerExecuteIntentRejectsInvalidCallWithoutSideEffects(t *testing.T) {
	t.Parallel()

	fixture := newHandlerFixture(t, downloader.IntentPlanned, "resource-invalid-call")
	got, err := fixture.handler.ExecuteIntent(t.Context(), " ", fixture.args.IntentID)
	if !errors.Is(err, ErrDownloadStateUnavailable) {
		t.Fatalf("ExecuteIntent() error = %v, want ErrDownloadStateUnavailable", err)
	}
	if got.Outcome != IntentExecutionRetryableFailure {
		t.Fatalf("ExecuteIntent() outcome = %q", got.Outcome)
	}
	if fixture.resolver.calls.Load() != 0 || fixture.submitter.addCalls.Load() != 0 {
		t.Fatal("invalid call reached external dependency")
	}
}

func containsSensitiveExecutionError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "passkey") ||
		strings.Contains(err.Error(), "secret") ||
		strings.Contains(err.Error(), "resource-serial"))
}
