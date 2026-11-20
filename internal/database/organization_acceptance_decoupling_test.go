package database

import (
	"strings"
	"testing"
)

func TestOrganizationAcceptanceDecouplingSQLCandidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
	}{
		{name: "create batch", sql: createOrganizationBatchSQL},
		{name: "claim due batches", sql: claimOrganizationBatchesSQL},
		{name: "claim one batch", sql: claimOrganizationBatchSQL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			candidate := compactOrganizationAcceptanceSQL(tt.sql)
			if before, _, found := strings.Cut(candidate, "locked_parents AS MATERIALIZED"); found {
				candidate = before
			}
			for _, fragment := range []string{
				"di.state='completed'",
				"di.subscription_id IS NULL",
				"s.id IS NOT NULL",
			} {
				if !strings.Contains(candidate, fragment) {
					t.Fatalf("candidate SQL missing %q: %s", fragment, candidate)
				}
			}
			for _, fragment := range []string{
				"s.completed_at IS NULL",
				"s.current_download_intent_id=di.id",
				"s.internal_state IN",
				"subscription_episodes",
			} {
				if strings.Contains(candidate, fragment) {
					t.Fatalf("candidate SQL still filters on %q: %s", fragment, candidate)
				}
			}
		})
	}
}

func TestOrganizationAcceptanceDecouplingClaimSQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
	}{
		{name: "claim due batches", sql: claimOrganizationBatchesSQL},
		{name: "claim one batch", sql: claimOrganizationBatchSQL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query := compactOrganizationAcceptanceSQL(tt.sql)
			for _, fragment := range []string{
				"SELECT s.id AS subscription_id, di.id AS intent_id",
				"s.completed_at IS NULL AND s.current_download_intent_id=di.id",
				"s.internal_state IN ('pending_organization', 'organizing', 'pending_index', 'indexed')",
				"SELECT count(*) FROM locked_parents",
				"CROSS JOIN parent_gate",
				"JOIN locked_parents ON locked_parents.intent_id=claimed.intent_id",
				"se.subscription_id=locked_parents.subscription_id",
				"s.id=locked_parents.subscription_id",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatalf("claim SQL missing %q: %s", fragment, query)
				}
			}
		})
	}
}

func compactOrganizationAcceptanceSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}
