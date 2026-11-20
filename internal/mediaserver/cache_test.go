package mediaserver

import "testing"

func TestItemValidRejectsRowsV2WouldNotStore(t *testing.T) {
	t.Parallel()
	valid := Item{
		Server: "home", ItemID: "movie-1", ItemType: ItemTypeMovie,
		SeasonInfo: map[int][]int{1: {1, 2}}, LstModDate: "2026-09-29 12:00:00.000000",
	}
	if !valid.Valid() {
		t.Fatalf("Valid() = false, want true for %#v", valid)
	}
	for name, mutate := range map[string]func(Item) Item{
		"missing server":     func(item Item) Item { item.Server = "  "; return item },
		"missing item id":    func(item Item) Item { item.ItemID = ""; return item },
		"missing sync stamp": func(item Item) Item { item.LstModDate = ""; return item },
		"unknown item type":  func(item Item) Item { item.ItemType = "动画"; return item },
		"negative season":    func(item Item) Item { item.SeasonInfo = map[int][]int{-1: {1}}; return item },
		"episode zero":       func(item Item) Item { item.SeasonInfo = map[int][]int{1: {0}}; return item },
		"episode overflow":   func(item Item) Item { item.SeasonInfo = map[int][]int{1: {5001}}; return item },
	} {
		if mutate(valid).Valid() {
			t.Errorf("%s: Valid() = true, want false", name)
		}
	}
	if !(Item{Server: "home", ItemID: "series-1", ItemType: ItemTypeTV,
		LstModDate: "stamp", SeasonInfo: nil}).Valid() {
		t.Error("TV rows without season info are valid in V2")
	}
}

func TestQueryEmptyMatchesV2LookupPrecedence(t *testing.T) {
	t.Parallel()
	if !(Query{}).Empty() {
		t.Error("Query{}.Empty() = false, want true")
	}
	if (Query{Title: "  "}).Empty() != true {
		t.Error("blank title without a TMDB ID must be an empty query")
	}
	if (Query{TMDBID: 123}).Empty() {
		t.Error("a truthy TMDB ID is never empty")
	}
	if (Query{Title: "Movie"}).Empty() {
		t.Error("a title alone is a valid V2 lookup")
	}
	// The movie caller passes the stored Chinese type and no year.
	movie := Query{TMDBID: 123, ItemType: ItemTypeMovie, Title: "Movie"}
	if movie.ItemType != ItemTypeMovie || movie.Year != "" {
		t.Fatalf("movie query fixture = %#v", movie)
	}
}
