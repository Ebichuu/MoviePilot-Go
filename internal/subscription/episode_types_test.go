package subscription

import "testing"

func TestSubscriptionEpisodeStateValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state SubscriptionEpisodeState
		want  string
	}{
		{name: "missing", state: SubscriptionEpisodeMissing, want: "missing"},
		{name: "acquired", state: SubscriptionEpisodeAcquired, want: "acquired"},
		{name: "downloading", state: SubscriptionEpisodeDownloading, want: "downloading"},
		{name: "pending organization", state: SubscriptionEpisodePendingOrganization, want: "pending_organization"},
		{name: "organizing", state: SubscriptionEpisodeOrganizing, want: "organizing"},
		{name: "pending index", state: SubscriptionEpisodePendingIndex, want: "pending_index"},
		{name: "indexed", state: SubscriptionEpisodeIndexed, want: "indexed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := string(test.state); got != test.want {
				t.Fatalf("SubscriptionEpisodeState = %q, want %q", got, test.want)
			}
		})
	}
}
