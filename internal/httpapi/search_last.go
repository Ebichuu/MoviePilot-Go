package httpapi

import (
	"net/http"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// lastSearchSource 返回最近一次成功搜索的结果与参数（可能为 nil）。
func (d Dependencies) lastSearchSource() *indexer.Coordinator {
	return d.Search
}

// lastSearch 复刻 V2 `/search/last`（app/api/endpoints/search.py:352–358）。
func (d Dependencies) lastSearch(w http.ResponseWriter, r *http.Request) {
	source := d.lastSearchSource()
	if source == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, searchContexts(source.LastSearchResults(), d.Config.Location))
}

// lastSearchContext 复刻 V2 `/search/last/context`（app/api/endpoints/search.py:361–380）。
func (d Dependencies) lastSearchContext(w http.ResponseWriter, r *http.Request) {
	source := d.lastSearchSource()
	if source == nil {
		writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{
			"params": map[string]any{}, "results": []any{},
		}})
		return
	}
	query := source.LastSearchQuery()
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{
		"params": map[string]any{
			"keyword": query.Keyword,
			"page":    query.Page,
			"limit":   query.Limit,
		},
		"results": searchContexts(source.LastSearchResults(), d.Config.Location),
	}})
}
