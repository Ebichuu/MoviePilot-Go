package subscription

import (
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

func TestExtendedBuiltinFilterRules(t *testing.T) {
	zero := 0.0
	half := 0.5
	two := 2.0
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	groups := []V2FilterRuleGroup{
		{Name: "ext", RuleString: "2XFREE & AV1 & !HR > H50 & IMAX & !HR > WEB & DV & HDR10P"},
	}

	c1 := Candidate{
		Key: "1",
		Resource: indexer.Resource{
			Title:                "Dune.2021.2160p.AV1.BluRay",
			DownloadVolumeFactor: &zero,
			UploadVolumeFactor:   &two,
			HitAndRun:            false,
		},
	}
	c2 := Candidate{
		Key: "2",
		Resource: indexer.Resource{
			Title:                "Dune.2021.2160p.IMAX.WEB-DL",
			DownloadVolumeFactor: &half,
			HitAndRun:            false,
		},
	}
	c3HR := Candidate{
		Key: "3",
		Resource: indexer.Resource{
			Title:                "Dune.2021.2160p.AV1.BluRay",
			DownloadVolumeFactor: &zero,
			UploadVolumeFactor:   &two,
			HitAndRun:            true, // excluded by !HR
		},
	}
	c4 := Candidate{
		Key: "4",
		Resource: indexer.Resource{
			Title: "Dune.2021.2160p.WEB-DL.DV.HDR10+",
		},
	}

	got, err := FilterV2Candidates([]Candidate{c1, c2, c3HR, c4}, []string{"ext"}, groups, nil, V2FilterMedia{}, now)
	if err != nil {
		t.Fatalf("FilterV2Candidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 matched (HR excluded), got %d: %+v", len(got), got)
	}
	if got[0].Key != "1" || got[0].Resource.PriOrder != 100 ||
		got[1].Key != "2" || got[1].Resource.PriOrder != 99 ||
		got[2].Key != "4" || got[2].Resource.PriOrder != 98 {
		t.Fatalf("unexpected priorities: %+v", got)
	}
}

func TestDefaultBestVersionRuleProgressionAndCompletion(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	webdl1080 := Candidate{Key: "web1080", Resource: indexer.Resource{Title: "Dune.2021.1080p.WEB-DL.H265.中字"}}
	remux4k := Candidate{Key: "remux4k", Resource: indexer.Resource{Title: "Dune.2021.2160p.UHD.BluRay.REMUX.HEVC.简繁中字"}}
	bdmv4k := Candidate{Key: "bdmv4k", Resource: indexer.Resource{Title: "Dune.2021.2160p.UHD.Blu-Ray.HEVC.BD100.中字"}}

	scored := ScoreDefaultBestVersionCandidates([]Candidate{webdl1080, remux4k, bdmv4k}, V2FilterMedia{}, now)
	if len(scored) != 3 {
		t.Fatalf("scored len = %d", len(scored))
	}
	if !(scored[2].Resource.PriOrder == 100 && scored[1].Resource.PriOrder == 99 && scored[0].Resource.PriOrder < scored[1].Resource.PriOrder) {
		t.Fatalf("unexpected best-version tier ordering: web1080=%d remux4k=%d bdmv4k=%d",
			scored[0].Resource.PriOrder, scored[1].Resource.PriOrder, scored[2].Resource.PriOrder)
	}

	// Simulate wash progression: start at 1080p WEB-DL -> upgrade to 4K REMUX (not terminal) -> upgrade to 4K BDMV (terminal 100)
	cur := scored[0].Resource.PriOrder
	evalRemux, err := EvaluateBestVersionUpgrade(remux4k, &cur, nil, nil, nil, V2FilterMedia{}, now)
	if err != nil || !evalRemux.Matched || !evalRemux.UpgradeAllowed || evalRemux.WillComplete || evalRemux.Rank != 2 {
		t.Fatalf("unexpected remux eval: %+v err=%v", evalRemux, err)
	}

	// Same or lower tier must be rejected
	cur = evalRemux.PriOrder
	evalDowngrade, err := EvaluateBestVersionUpgrade(webdl1080, &cur, nil, nil, nil, V2FilterMedia{}, now)
	if err != nil || evalDowngrade.UpgradeAllowed {
		t.Fatalf("expected downgrade blocked: %+v err=%v", evalDowngrade, err)
	}

	// Top tier (100) upgrades and completes wash
	evalTop, err := EvaluateBestVersionUpgrade(bdmv4k, &cur, nil, nil, nil, V2FilterMedia{}, now)
	if err != nil || !evalTop.Matched || !evalTop.UpgradeAllowed || !evalTop.WillComplete || evalTop.Rank != 1 {
		t.Fatalf("unexpected top eval: %+v err=%v", evalTop, err)
	}
}
