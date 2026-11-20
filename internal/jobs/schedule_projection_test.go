package jobs

import (
	"testing"
	"time"
)

func TestScheduledJobsProjection(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	manager := &Manager{
		startedAt:                  base,
		rssSweepEnabled:            true,
		rssPeriodicInterval:        15 * time.Minute,
		subscriptionSearchEnabled:  true,
		downloadReconcileEnabled:   true,
		organizationSweepEnabled:   true,
		siteUserDataRefreshEnabled: true,
		siteSigninSweepEnabled:     true,
		siteSigninPeriodicInterval: 12 * time.Hour,
		backupSweepEnabled:         true,
		backupPeriodicInterval:     24 * time.Hour,
		cleanupSweepEnabled:        true,
		mediaServerSyncEnabled:     true,
		mediaServerPeriodicSchedules: []MediaServerSyncSchedule{
			{Server: "EmbyMain", Interval: 2 * time.Hour},
		},
	}

	now := base.Add(20 * time.Minute)
	items := manager.ScheduledJobs(now)
	if len(items) != 9 {
		t.Fatalf("len(ScheduledJobs) = %d, want 9: %+v", len(items), items)
	}

	byID := make(map[string]ScheduledJob, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}

	rss, ok := byID["subscribe_refresh"]
	if !ok {
		t.Fatal("missing subscribe_refresh schedule")
	}
	wantNextRSS := base.Add(30 * time.Minute)
	if !rss.NextRun.Equal(wantNextRSS) {
		t.Fatalf("rss NextRun = %v, want %v", rss.NextRun, wantNextRSS)
	}

	emby, ok := byID["mediaserver_sync:EmbyMain"]
	if !ok {
		t.Fatal("missing mediaserver_sync:EmbyMain schedule")
	}
	if emby.Interval != 2*time.Hour {
		t.Fatalf("emby interval = %v, want 2h", emby.Interval)
	}
}
