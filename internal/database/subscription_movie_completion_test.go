package database

import (
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMovieTerminalQualityCompletionKeepsCASAndTVBoundary(t *testing.T) {
	t.Parallel()
	query, args, err := buildCompleteSatisfiedMovieSubscription("owner", 1, subscription.Active, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if query != completeSatisfiedMovieSubscriptionSQL || args[6] != "{}" {
		t.Fatalf("movie completion query=%s patch=%v", query, args[6])
	}
	for _, required := range []string{"id=$1 AND username=$2", "completed_at IS NULL", "current_download_intent_id IS NULL", "internal_state=$5", "updated_at=$6", "jsonb_typeof(config)='object'", "LOWER(BTRIM(media_type)) IN ($8, $9)", "jsonb_typeof(config->'current_priority')", "(config->>'current_priority')::numeric = 100"} {
		if !strings.Contains(query, required) {
			t.Fatalf("missing CAS/terminal guard %s", required)
		}
	}
	tvQuery, _, err := buildCompleteSatisfiedTVSubscription("owner", 1, subscription.Active, time.Now())
	if err != nil || tvQuery != completeSatisfiedSubscriptionSQL || strings.Contains(tvQuery, "current_priority") {
		t.Fatal("movie terminal completion widened TV predicate")
	}
}
