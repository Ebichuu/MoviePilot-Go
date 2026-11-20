package metadata

import "testing"

func TestV2CategoryUsesDefaultCategoryYAMLOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mediaType string
		item      Media
		want      string
	}{
		{name: "animated Chinese movie uses first rule", mediaType: "电影", item: Media{GenreIDs: []int{16}, OriginalLanguage: "zh"}, want: "动画电影"},
		{name: "Chinese movie", mediaType: "movie", item: Media{OriginalLanguage: "cn"}, want: "华语电影"},
		{name: "foreign movie fallback", mediaType: "电影", item: Media{OriginalLanguage: "en"}, want: "外语电影"},
		{name: "Chinese animation", mediaType: "电视剧", item: Media{GenreIDs: []int{16}, OriginCountries: []string{"CN"}}, want: "国漫"},
		{name: "Japanese animation", mediaType: "tv", item: Media{GenreIDs: []int{16}, OriginCountries: []string{"JP"}}, want: "日番"},
		{name: "documentary precedes country", mediaType: "电视剧", item: Media{GenreIDs: []int{99}, OriginCountries: []string{"US"}}, want: "纪录片"},
		{name: "variety", mediaType: "电视剧", item: Media{GenreIDs: []int{10767}}, want: "综艺"},
		{name: "domestic drama", mediaType: "电视剧", item: Media{OriginCountries: []string{"HK"}}, want: "国产剧"},
		{name: "European drama", mediaType: "电视剧", item: Media{OriginCountries: []string{"GB"}}, want: "欧美剧"},
		{name: "Asian drama", mediaType: "电视剧", item: Media{OriginCountries: []string{"KR"}}, want: "日韩剧"},
		{name: "TV fallback", mediaType: "电视剧", item: Media{}, want: "未分类"},
		{name: "unknown media type", mediaType: "动漫", item: Media{GenreIDs: []int{16}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := V2Category(tt.mediaType, tt.item); got != tt.want {
				t.Fatalf("V2Category() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEvaluateV2CategoryPreservesV2RuleQuirks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rule v2CategoryRule
		item Media
		want string
	}{
		{name: "conditions are ANDed", rule: v2CategoryRule{name: "match", conditions: []v2CategoryCondition{{"genre_ids", "16"}, {"origin_country", "JP"}}}, item: Media{GenreIDs: []int{16}, OriginCountries: []string{"JP"}}, want: "match"},
		{name: "missing fact fails inverted-only condition", rule: v2CategoryRule{name: "missing", conditions: []v2CategoryCondition{{"origin_country", "!US"}}}, item: Media{}},
		{name: "inverted value excludes intersection", rule: v2CategoryRule{name: "excluded", conditions: []v2CategoryCondition{{"origin_country", "!US"}}}, item: Media{OriginCountries: []string{"US"}}},
		{name: "inverted value permits other fact", rule: v2CategoryRule{name: "allowed", conditions: []v2CategoryCondition{{"origin_country", "!US"}}}, item: Media{OriginCountries: []string{"JP"}}, want: "allowed"},
		{name: "numeric closed range", rule: v2CategoryRule{name: "range", conditions: []v2CategoryCondition{{"release_year", "2020-2026"}}}, item: Media{Year: 2024}, want: "range"},
		{name: "billion-wide numeric range matches without expansion", rule: v2CategoryRule{name: "large", conditions: []v2CategoryCondition{{"release_year", "1-1000000000"}}}, item: Media{Year: 999999999}, want: "large"},
		{name: "billion-wide numeric range rejects outside value", rule: v2CategoryRule{name: "outside", conditions: []v2CategoryCondition{{"release_year", "1-1000000000"}}}, item: Media{Year: 1000000001}},
		{name: "billion-wide inverted range excludes inside value", rule: v2CategoryRule{name: "excluded", conditions: []v2CategoryCondition{{"release_year", "!1-1000000000"}}}, item: Media{Year: 999999999}},
		{name: "billion-wide inverted range permits outside value", rule: v2CategoryRule{name: "allowed", conditions: []v2CategoryCondition{{"release_year", "!1-1000000000"}}}, item: Media{Year: 1000000001}, want: "allowed"},
		{name: "reversed numeric range expands empty and passes", rule: v2CategoryRule{name: "reverse", conditions: []v2CategoryCondition{{"release_year", "2026-2020"}}}, item: Media{Year: 2024}, want: "reverse"},
		{name: "reversed inverted numeric range also passes", rule: v2CategoryRule{name: "reverse", conditions: []v2CategoryCondition{{"release_year", "!2026-2020"}}}, item: Media{Year: 2024}, want: "reverse"},
		{name: "numeric range drops leading zeros like Python range", rule: v2CategoryRule{name: "zeroes", conditions: []v2CategoryCondition{{"release_year", "002020-002026"}}}, item: Media{Year: 2024}, want: "zeroes"},
		{name: "non-numeric range uses endpoints", rule: v2CategoryRule{name: "endpoints", conditions: []v2CategoryCondition{{"origin_country", "CN-US"}}}, item: Media{OriginCountries: []string{"US"}}, want: "endpoints"},
		{name: "configuration values are not trimmed", rule: v2CategoryRule{name: "spacing", conditions: []v2CategoryCondition{{"origin_country", "CN, US"}}}, item: Media{OriginCountries: []string{"US"}}},
		{name: "production country matches ISO code", rule: v2CategoryRule{name: "country", conditions: []v2CategoryCondition{{"production_countries", "US"}}}, item: Media{ProductionCountries: []string{"US"}}, want: "country"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := evaluateV2Category([]v2CategoryRule{tt.rule}, tt.item); got != tt.want {
				t.Fatalf("evaluateV2Category() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestV2CategoryLargeNumericRangeUsesBoundedAllocations(t *testing.T) {
	rules := []v2CategoryRule{{name: "large", conditions: []v2CategoryCondition{{"release_year", "1-1000000000"}}}}
	allocations := testing.AllocsPerRun(100, func() {
		if got := evaluateV2Category(rules, Media{Year: 500000000}); got != "large" {
			panic(got)
		}
	})
	if allocations > 20 {
		t.Fatalf("十亿跨度范围每次分配 %.1f 次，期望不随区间宽度展开", allocations)
	}
}

func BenchmarkEvaluateV2CategoryLargeNumericRange(b *testing.B) {
	rules := []v2CategoryRule{{name: "large", conditions: []v2CategoryCondition{{"release_year", "1-1000000000"}}}}
	item := Media{Year: 500000000}
	b.ReportAllocs()
	for range b.N {
		if evaluateV2Category(rules, item) != "large" {
			b.Fatal("unexpected mismatch")
		}
	}
}
