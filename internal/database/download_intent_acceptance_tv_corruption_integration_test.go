//go:build integration

package database

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestTVQualityAcceptancePostgresPersistsIrreversibleAcceptanceWithCorruptFacts(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-accept-corruption"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-accept-corruption-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	for index, tc := range []struct {
		name     string
		config   string
		coverage string
	}{
		{name: "坏 total 类型", config: `{"best_version":true,"total_episode":"oops","note":[1]}`},
		{name: "越界 total", config: `{"best_version":true,"total_episode":5001,"note":[1]}`},
		{name: "坏 current 类型", config: `{"best_version":true,"total_episode":2,"current_priority":"oops","note":[1]}`},
		{name: "质量集号别名冲突", config: `{"best_version":true,"total_episode":2,"episode_priority":{"1":90,"01":100},"note":[1]}`},
		{name: "非法计划集号", coverage: `[{"season":1,"episode":0}]`},
		{name: "非法计划季号", coverage: `[{"season":-1,"episode":1}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("tv-corrupt-%d", index)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, episodes, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage = r08MoviePriority(100), episodes
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			config := tc.config
			if config == "" {
				config = `{"best_version":true,"total_episode":2,"note":[1]}`
			}
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET season=1,config=$2::jsonb WHERE id=$1`, fixture.subscriptionID, config); err != nil {
				t.Fatal(err)
			}
			if tc.coverage != "" {
				// Migration 039 guards the JSON container; per-coordinate validation
				// happens on read, so these rows model a legacy or manual bad write.
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE download_intent_plans SET tv_coverage=$2::jsonb WHERE intent_id=$1`, fixture.intent.ID, tc.coverage); err != nil {
					t.Fatal(err)
				}
				if _, err := store.GetDownloadIntentPlan(t.Context(), owner, fixture.intent.ID); !errors.Is(err, ErrDownloadIntentPlanPersistence) {
					t.Fatalf("坏计划读取未失败: %v", err)
				}
			}
			beforeParent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			beforeConfig := readTVQualityConfig(t, store, fixture.subscriptionID)
			beforeEpisodes := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
			accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || released || accepted.State != downloader.IntentDownloading || accepted.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("不可逆接受事实被坏进度阻塞: intent=%+v released=%v err=%v", accepted, released, err)
			}
			stored, err := store.GetDownloadIntent(t.Context(), owner, fixture.intent.ID)
			if err != nil || stored.State != downloader.IntentDownloading || stored.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("接受事实未落库: intent=%+v err=%v", stored, err)
			}
			if !reflect.DeepEqual(beforeParent, readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)) ||
				!reflect.DeepEqual(beforeConfig, readTVQualityConfig(t, store, fixture.subscriptionID)) ||
				!reflect.DeepEqual(beforeEpisodes, readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)) {
				t.Fatal("无法解释质量事实时消耗了父订阅或逐集事实")
			}
			// A later repair can retry settlement without another external Add.
			if tc.coverage != "" {
				if _, err := store.db.Pool.Exec(t.Context(), `UPDATE download_intent_plans SET tv_coverage='[{"season":1,"episode":1},{"season":1,"episode":2}]'::jsonb WHERE intent_id=$1`, fixture.intent.ID); err != nil {
					t.Fatal(err)
				}
			}
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{"best_version": true, "total_episode": 2, "note": []int{1}})
			if _, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released {
				t.Fatalf("修复后接受事实不能重放结算: released=%v err=%v", released, err)
			}
			if after := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID); after.state != subscription.Completed || after.currentIntent != nil || !after.completed {
				t.Fatalf("修复后父订阅=%+v", after)
			}
		})
	}
}

func TestTVQualityAcceptancePostgresUsesCurrentModeAndCurrentIntentCoverage(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-accept-mode-change"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-accept-mode-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	for index, tc := range []struct {
		name        string
		best        bool
		bound       []subscription.Episode
		actual      []subscription.Episode
		confirmed   bool
		note        []int
		priority    int
		wantState   subscription.State
		wantLack    int
		wantCurrent int
	}{
		{"洗版改普通全覆盖", false, nil, episodes, false, nil, 100, subscription.Completed, 0, 30},
		{"洗版改普通 confirmed 空覆盖回填当前目标", false, nil, []subscription.Episode{}, true, nil, 100, subscription.Completed, 0, 30},
		{"洗版改普通仅扣本意图覆盖不按旧 note 推完成", false, nil, episodes[1:], false, []int{1}, 100, subscription.PendingSearch, 1, 30},
		{"普通改洗版按质量完成而非逐集状态", true, episodes, episodes, false, nil, 90, subscription.Active, 0, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("mode-change-%d", index)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, tc.bound, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage, plan.TVFullCoverage = r08MoviePriority(tc.priority), tc.actual, tc.confirmed
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{
				"best_version": tc.best, "best_version_full": false, "total_episode": 2, "start_episode": 1,
				"lack_episode": 2, "note": tc.note, "current_priority": 30,
			})
			beforeEpisodes := readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)
			if accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !released || accepted.State != downloader.IntentDownloading {
				t.Fatalf("切换模式后接受: intent=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			config := readTVQualityConfig(t, store, fixture.subscriptionID)
			if parent.state != tc.wantState || parent.currentIntent != nil || config["lack_episode"] != float64(tc.wantLack) || config["current_priority"] != float64(tc.wantCurrent) {
				t.Fatalf("切换模式后结算: parent=%+v config=%+v", parent, config)
			}
			if tc.bound == nil && !reflect.DeepEqual(beforeEpisodes, readDownloadIntentAcceptanceEpisodeStates(t, store, fixture.subscriptionID)) {
				t.Fatal("独立覆盖更改了没有绑定到本意图的逐集事实")
			}
		})
	}
}

func TestTVQualityAcceptancePostgresWriterUsesImmutableMediaIdentity(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-accept-identity-site")
	episodes := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	for index, tc := range []struct {
		name                         string
		parentSource, downloadSource string
		parentID, downloadType       string
		full, ordinary, legacy       bool
		deny                         bool
	}{
		{name: "TMDB 改媒体拒绝旧质量", parentSource: "themoviedb", downloadSource: "tmdb", parentID: "43", deny: true},
		{name: "豆瓣改媒体拒绝旧质量", parentSource: "douban", downloadSource: "douban", parentID: "43", deny: true},
		{name: "同ID不同TMDB别名仍接受", parentSource: "themoviedb", downloadSource: "tmdb", parentID: "42"},
		{name: "跨provider缺少同类ID仍接受", parentSource: "douban", downloadSource: "tmdb", parentID: "43"},
		{name: "整季ID变化不提升总质量", parentSource: "tmdb", downloadSource: "tmdb", parentID: "43", full: true, deny: true},
		{name: "普通ID变化不写writer但本轮lefts仍可完成", parentSource: "tmdb", downloadSource: "tmdb", parentID: "43", ordinary: true, deny: true},
		{name: "captured电影不能写TV质量", parentSource: "tmdb", downloadSource: "tmdb", parentID: "42", downloadType: "电影", deny: true},
		{name: "legacy无冻结身份不新增拒绝", parentSource: "tmdb", parentID: "43", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("tv-identity-%d", index)
			// 每个用例独立用户：身份唯一索引按用户生效，复用同一用户会让
			// 「把订阅改成相同 media_source/media_id」的前后用例互相撞键。
			owner := "tv-accept-identity-" + key
			insertDownloadIntentIntegrationUsers(t, store, owner)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", episodes, nil, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage, plan.TVFullCoverage = r08MoviePriority(100), episodes, true
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			if !tc.legacy {
				mediaType := tc.downloadType
				if mediaType == "" {
					mediaType = "电视剧"
				}
				createDownloadSuccessInputIntegration(t, store, owner, organization.SuccessInput{
					IntentID: fixture.intent.ID, Version: organization.CurrentSuccessVersion, Username: owner,
					Media:    organization.MediaContext{Source: tc.downloadSource, MediaID: "42", MediaType: mediaType, TitleYear: "TV (2026)"},
					Episodes: []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}},
				})
			}
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{
				"best_version": !tc.ordinary, "best_version_full": tc.full, "total_episode": 2,
				"current_priority": 90, "note": []int{1}, "episode_priority": map[string]int{"1": 90},
			})
			if _, err := store.db.Pool.Exec(t.Context(), `UPDATE subscriptions SET media_source=$2,media_id=$3 WHERE id=$1`, fixture.subscriptionID, tc.parentSource, tc.parentID); err != nil {
				t.Fatal(err)
			}
			before := readTVQualityConfig(t, store, fixture.subscriptionID)
			accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || !released || accepted.State != downloader.IntentDownloading || accepted.TorrentHash != databaseIntentV1Hash {
				t.Fatalf("身份writer过滤阻塞不可逆接受: intent=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			after := readTVQualityConfig(t, store, fixture.subscriptionID)
			if tc.deny {
				if !reflect.DeepEqual(before["note"], after["note"]) || !reflect.DeepEqual(before["episode_priority"], after["episode_priority"]) {
					t.Fatalf("身份变化写入旧事实: before=%+v after=%+v", before, after)
				}
				if tc.ordinary {
					if parent.state != subscription.Completed || after["lack_episode"] != float64(0) || after["current_priority"] != float64(90) {
						t.Fatalf("writer拒绝改写了ordinary本轮lefts行为: parent=%+v config=%+v", parent, after)
					}
				} else {
					wantCurrent := float64(0)
					if tc.full {
						wantCurrent = 90
					}
					if parent.state != subscription.Active || after["lack_episode"] != float64(1) || after["current_priority"] != wantCurrent {
						t.Fatalf("writer拒绝后best进度: parent=%+v config=%+v", parent, after)
					}
				}
			} else if parent.state != subscription.Completed || after["lack_episode"] != float64(0) || after["current_priority"] != float64(100) {
				t.Fatalf("原版允许的身份被拒绝: parent=%+v config=%+v", parent, after)
			}
			if _, replayed, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash); err != nil || !replayed || !reflect.DeepEqual(after, readTVQualityConfig(t, store, fixture.subscriptionID)) {
				t.Fatalf("身份过滤后重放改变质量: replayed=%v err=%v", replayed, err)
			}
		})
	}
}

func TestTVQualityAcceptancePostgresNoTargetCoveragePreservesRawFacts(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const owner = "tv-accept-no-target-coverage"
	insertDownloadIntentIntegrationUsers(t, store, owner)
	site := insertDownloadIntentPlanIntegrationSite(t, store, "tv-accept-no-target-site")
	for index, tc := range []struct {
		name         string
		actual       []subscription.Episode
		total, start int
		current      int
		wantState    subscription.State
		wantCurrent  int
		wantLack     int
	}{
		{"known empty 非confirmed", []subscription.Episode{}, 2, 1, 90, subscription.Active, 0, 1},
		{"实际覆盖没有当前目标交集", []subscription.Episode{{Season: 1, Episode: 3}}, 2, 1, 90, subscription.Active, 0, 1},
		{"空目标保留100终态总体质量", []subscription.Episode{}, 2, 3, 100, subscription.Completed, 100, 0},
		{"未知目标保留90总体质量", []subscription.Episode{}, 0, 1, 90, subscription.Active, 90, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("no-target-coverage-%d", index)
			fixture := prepareDownloadIntentAcceptanceFixture(t, store, owner, key, "电视剧", nil, nil, "")
			plan := downloadIntentPlanIntegrationFixture(fixture.intent.ID, site, key)
			plan.TVPriority, plan.TVCoverage = r08MoviePriority(100), tc.actual
			if _, err := store.CreateDownloadIntentPlan(t.Context(), owner, plan); err != nil {
				t.Fatal(err)
			}
			setTVQualityIntegrationConfig(t, store, fixture.subscriptionID, map[string]any{
				"best_version": true, "total_episode": tc.total, "start_episode": tc.start,
				"current_priority": tc.current, "note": []any{"junk", 1},
				"episode_priority": map[string]any{"foo": 90, "1": 90},
			})
			before := readTVQualityConfig(t, store, fixture.subscriptionID)
			accepted, released, err := store.AcceptDownloadIntent(t.Context(), owner, fixture.intent.ID, databaseIntentV1Hash)
			if err != nil || !released || accepted.State != downloader.IntentDownloading {
				t.Fatalf("无目标覆盖接受被阻塞: intent=%+v released=%v err=%v", accepted, released, err)
			}
			parent := readDownloadIntentAcceptanceParent(t, store, fixture.subscriptionID)
			after := readTVQualityConfig(t, store, fixture.subscriptionID)
			if parent.state != tc.wantState || after["current_priority"] != float64(tc.wantCurrent) || after["lack_episode"] != float64(tc.wantLack) {
				t.Fatalf("空目标或无交集进度不符原版: parent=%+v config=%+v", parent, after)
			}
			if !reflect.DeepEqual(before["note"], after["note"]) || !reflect.DeepEqual(before["episode_priority"], after["episode_priority"]) {
				t.Fatalf("本轮writer没有覆盖却重写raw事实: before=%+v after=%+v", before, after)
			}
		})
	}
}
