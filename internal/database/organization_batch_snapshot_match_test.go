package database

import (
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

// TestOrganizationSnapshotsMatchPlanMismatches 覆盖快照与计划比对的三类不一致：
// 序号越界/大小不符、重复序号、以及缺少计划中的文件。
func TestOrganizationSnapshotsMatchPlanMismatches(t *testing.T) {
	plan := organizer.Plan{Operations: []organizer.Operation{
		{ExpectedSize: 10},
		{ExpectedSize: 20},
	}}

	matched := []organization.SuccessSnapshot{{Files: []organization.FileFact{
		{Ordinal: 0, SizeBytes: 10},
		{Ordinal: 1, SizeBytes: 20},
	}}}
	if !organizationSnapshotsMatchPlan(matched, plan) {
		t.Fatal("完整快照应判为匹配")
	}

	cases := []struct {
		name      string
		snapshots []organization.SuccessSnapshot
	}{
		{
			name:      "序号越界",
			snapshots: []organization.SuccessSnapshot{{Files: []organization.FileFact{{Ordinal: 5, SizeBytes: 10}}}},
		},
		{
			name:      "大小不一致",
			snapshots: []organization.SuccessSnapshot{{Files: []organization.FileFact{{Ordinal: 0, SizeBytes: 99}, {Ordinal: 1, SizeBytes: 20}}}},
		},
		{
			name:      "重复序号",
			snapshots: []organization.SuccessSnapshot{{Files: []organization.FileFact{{Ordinal: 0, SizeBytes: 10}}}, {Files: []organization.FileFact{{Ordinal: 0, SizeBytes: 10}}}},
		},
		{
			name:      "缺少文件",
			snapshots: []organization.SuccessSnapshot{{Files: []organization.FileFact{{Ordinal: 0, SizeBytes: 10}}}},
		},
		{
			name:      "空快照",
			snapshots: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if organizationSnapshotsMatchPlan(tc.snapshots, plan) {
				t.Fatal("应判为不匹配")
			}
		})
	}
}
