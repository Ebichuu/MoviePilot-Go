package emby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SystemInfo returns authenticated Emby server information.
func (c *Client) SystemInfo(ctx context.Context) (SystemInfo, error) {
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return SystemInfo{}, err
	}
	defer cancel()

	var result SystemInfo
	if err := c.getJSON(ctx, c.endpoint("/System/Info"), maxSystemInfoBytes, &result); err != nil {
		return SystemInfo{}, err
	}
	return result, nil
}

// ListLibraries returns Emby's configured virtual folders.
func (c *Client) ListLibraries(ctx context.Context) ([]Library, error) {
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var result []Library
	if err := c.getJSON(ctx, c.endpoint("/Library/VirtualFolders"), maxLibrariesBytes, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// FindItems performs one bounded Movie or Episode search page.
func (c *Client) FindItems(ctx context.Context, options FindItemsOptions) (ItemsResponse, error) {
	limit, fields, err := validateFindItemsOptions(options)
	if err != nil {
		return ItemsResponse{}, err
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return ItemsResponse{}, err
	}
	defer cancel()

	query := make(url.Values)
	if options.Path != "" {
		query.Set("Path", options.Path)
	}
	if options.AnyProviderIDEquals != "" {
		query.Set("AnyProviderIdEquals", options.AnyProviderIDEquals)
	}
	query.Set("IncludeItemTypes", "Movie,Episode")
	query.Set("Recursive", strconv.FormatBool(options.Recursive))
	if fields != "" {
		query.Set("Fields", fields)
	}
	if options.StartIndex > 0 {
		query.Set("StartIndex", strconv.Itoa(options.StartIndex))
	}
	query.Set("Limit", strconv.Itoa(limit))

	var result ItemsResponse
	if err := c.getJSON(ctx, c.endpoint("/Items")+"?"+query.Encode(), maxItemsBytes, &result); err != nil {
		return ItemsResponse{}, err
	}
	return result, nil
}

// SearchSeries performs the same bounded title lookup used by MoviePilot V2
// before it asks Emby for a show's episodes.
func (c *Client) SearchSeries(ctx context.Context, options SearchSeriesOptions) (ItemsResponse, error) {
	searchTerm := strings.TrimSpace(options.SearchTerm)
	limit := options.Limit
	if limit == 0 {
		limit = maxSeriesSearchLimit
	}
	if searchTerm == "" || len(searchTerm) > 4096 || strings.ContainsAny(searchTerm, "\r\n") ||
		limit < 1 || limit > maxSeriesSearchLimit {
		return ItemsResponse{}, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return ItemsResponse{}, err
	}
	defer cancel()

	query := url.Values{
		"IncludeItemTypes":   {"Series"},
		"Fields":             {"ProductionYear,ProviderIds"},
		"StartIndex":         {"0"},
		"Recursive":          {"true"},
		"SearchTerm":         {searchTerm},
		"Limit":              {strconv.Itoa(limit)},
		"IncludeSearchTypes": {"false"},
	}
	var result ItemsResponse
	if err := c.getJSON(ctx, c.endpoint("/Items")+"?"+query.Encode(), maxItemsBytes, &result); err != nil {
		return ItemsResponse{}, err
	}
	return result, nil
}

// SearchMovies performs the bounded title lookup used by MoviePilot V2 when
// checking whether a movie subscription is already satisfied by Emby.
func (c *Client) SearchMovies(ctx context.Context, options SearchMoviesOptions) (ItemsResponse, error) {
	searchTerm := strings.TrimSpace(options.SearchTerm)
	limit := options.Limit
	if limit == 0 {
		limit = maxMovieSearchLimit
	}
	if searchTerm == "" || len(searchTerm) > 4096 || strings.ContainsAny(searchTerm, "\r\n") ||
		limit < 1 || limit > maxMovieSearchLimit {
		return ItemsResponse{}, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return ItemsResponse{}, err
	}
	defer cancel()

	query := url.Values{
		"IncludeItemTypes":   {"Movie"},
		"Fields":             {"ProviderIds,OriginalTitle,ProductionYear,Path,UserDataPlayCount,UserDataLastPlayedDate,ParentId"},
		"StartIndex":         {"0"},
		"Recursive":          {"true"},
		"SearchTerm":         {searchTerm},
		"Limit":              {strconv.Itoa(limit)},
		"IncludeSearchTypes": {"false"},
	}
	var result ItemsResponse
	if err := c.getJSON(ctx, c.endpoint("/Items")+"?"+query.Encode(), maxItemsBytes, &result); err != nil {
		return ItemsResponse{}, err
	}
	return result, nil
}

// ListEpisodes returns one bounded page from Emby's show episode endpoint.
func (c *Client) ListEpisodes(ctx context.Context, options ListEpisodesOptions) (ItemsResponse, error) {
	seriesID := strings.TrimSpace(options.SeriesID)
	limit := options.Limit
	if limit == 0 {
		limit = maxFindItemsLimit
	}
	if seriesID == "" || len(seriesID) > 4096 || strings.ContainsAny(seriesID, "\r\n") ||
		limit < 1 || limit > maxFindItemsLimit || options.StartIndex < 0 ||
		options.StartIndex > maxFindItemsStartIndex {
		return ItemsResponse{}, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return ItemsResponse{}, err
	}
	defer cancel()

	query := url.Values{
		"IsMissing": {"false"},
		"Limit":     {strconv.Itoa(limit)},
	}
	if options.StartIndex > 0 {
		query.Set("StartIndex", strconv.Itoa(options.StartIndex))
	}
	var result ItemsResponse
	endpoint := c.endpoint("/Shows/" + url.PathEscape(seriesID) + "/Episodes")
	if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), maxItemsBytes, &result); err != nil {
		return ItemsResponse{}, err
	}
	return result, nil
}

// RefreshItem asks Emby to refresh one item. It is never automatically retried.
func (c *Client) RefreshItem(ctx context.Context, itemID string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" || len(itemID) > 4096 || strings.ContainsAny(itemID, "\r\n") {
		return newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	return c.postRefresh(ctx, c.endpoint("/Items/"+url.PathEscape(itemID)+"/Refresh"))
}

// RefreshAll asks Emby to refresh all libraries. It is never automatically retried.
func (c *Client) RefreshAll(ctx context.Context) error {
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	return c.postRefresh(ctx, c.endpoint("/Library/Refresh"))
}

// PlayURL returns the Emby or Jellyfin web client item URL while preserving
// PlayHost's configured base path.
func (c *Client) PlayURL(itemID string) (string, error) {
	itemID = strings.TrimSpace(itemID)
	if c == nil || c.playHost == nil || itemID == "" || len(itemID) > 4096 || strings.ContainsAny(itemID, "\r\n") {
		return "", newError(ErrorCodeInvalidArgument, 0, nil)
	}
	fragmentQuery := url.Values{"id": {itemID}}.Encode()
	route := "/web/index.html#!/item?"
	if c.serverType == "jellyfin" {
		route = "/web/index.html#!/details?"
	}
	return strings.TrimRight(c.playHost.String(), "/") + route + fragmentQuery, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, maxBodyBytes int64, target any) error {
	body, err := c.get(ctx, endpoint, maxBodyBytes)
	if err != nil {
		return err
	}
	if len(body) == 0 || json.Unmarshal(body, target) != nil {
		return newError(ErrorCodeInvalidResponse, 0, nil)
	}
	return nil
}

func (c *Client) get(ctx context.Context, endpoint string, maxBodyBytes int64) ([]byte, error) {
	for attempt := 0; attempt < maxGETAttempts; attempt++ {
		response, _, err := c.doOnce(ctx, http.MethodGet, endpoint, maxBodyBytes)
		if err == nil && response.statusCode >= http.StatusOK && response.statusCode < http.StatusMultipleChoices {
			return response.body, nil
		}
		if err != nil {
			if !shouldRetryTransport(err, attempt) {
				return nil, err
			}
			if waitErr := waitForRetry(ctx, retryDelay(attempt, "")); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if !retryableStatus(response.statusCode) || attempt+1 == maxGETAttempts {
			return nil, statusError(response.statusCode)
		}
		if waitErr := waitForRetry(ctx, retryDelay(attempt, response.retryAfter)); waitErr != nil {
			return nil, waitErr
		}
	}
	return nil, newError(ErrorCodeTransport, 0, nil)
}

func (c *Client) postRefresh(ctx context.Context, endpoint string) error {
	response, started, err := c.doOnce(ctx, http.MethodPost, endpoint, maxRefreshBytes)
	if err != nil {
		if started && (err.Code == ErrorCodeTransport || err.Code == ErrorCodeCanceled) {
			return newError(ErrorCodeOutcomeUnknown, 0, err.cause)
		}
		return err
	}
	if response.statusCode < http.StatusOK || response.statusCode >= http.StatusMultipleChoices {
		return statusError(response.statusCode)
	}
	return nil
}

func validateFindItemsOptions(options FindItemsOptions) (int, string, error) {
	limit := options.Limit
	if limit == 0 {
		limit = 2
	}
	if limit < 1 || limit > maxFindItemsLimit || options.StartIndex < 0 ||
		options.StartIndex > maxFindItemsStartIndex || len(options.Path) > 64<<10 ||
		len(options.AnyProviderIDEquals) > 64<<10 {
		return 0, "", newError(ErrorCodeInvalidArgument, 0, nil)
	}
	fields, err := encodeFields(options.Fields)
	if err != nil {
		return 0, "", err
	}
	return limit, fields, nil
}

func encodeFields(fields []string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	encoded := make([]string, 0, len(fields))
	total := 0
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || strings.ContainsAny(field, ",\r\n") {
			return "", newError(ErrorCodeInvalidArgument, 0, nil)
		}
		total += len(field)
		if total > 4096 {
			return "", newError(ErrorCodeInvalidArgument, 0, nil)
		}
		encoded = append(encoded, field)
	}
	return strings.Join(encoded, ","), nil
}

func shouldRetryTransport(err *Error, attempt int) bool {
	return err.Code == ErrorCodeTransport && err.temporary && attempt+1 < maxGETAttempts
}

func retryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func statusError(statusCode int) *Error {
	if statusCode == http.StatusTooManyRequests {
		return newError(ErrorCodeRateLimited, statusCode, nil)
	}
	return newError(ErrorCodeHTTPStatus, statusCode, nil)
}
