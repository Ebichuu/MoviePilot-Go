package emby

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxUsersBytes          int64 = 1 << 20
	maxUserViewsBytes      int64 = 1 << 20
	maxItemDetailBytes     int64 = 256 << 10
	maxUserItemsPageBytes  int64 = 512 << 10
	maxUserItemsPageLimit        = 200
	maxUserItemsStartIndex       = 10_000
	maxEmbyPathIDBytes           = 4096
)

const v2LibraryItemFields = "ProviderIds,OriginalTitle,ProductionYear,Path,UserDataPlayCount,UserDataLastPlayedDate,ParentId"

// User is the subset returned by Emby's public user list that MoviePilot V2
// uses to resolve the user-scoped item endpoint.
type User struct {
	ID     string     `json:"Id"`
	Name   string     `json:"Name"`
	Policy UserPolicy `json:"Policy"`
}

type UserPolicy struct {
	IsAdministrator bool `json:"IsAdministrator"`
}

// UserItemsOptions selects one bounded page below an Emby parent folder.
type UserItemsOptions struct {
	UserID     string
	ParentID   string
	StartIndex int
	Limit      int
}

// ListUsers returns the bounded user list used by V2's get_user routine.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var users []User
	if err := c.getJSON(ctx, c.endpoint("/Users"), maxUsersBytes, &users); err != nil {
		return nil, err
	}
	if users == nil {
		users = []User{}
	}
	return users, nil
}

// ResolveUserID reproduces V2 get_user ordering without guessing where the
// caller's preferred username came from: requested first, configured second,
// then the first administrator in Emby's response order.
func ResolveUserID(users []User, requestedUsername, configuredUsername string) string {
	candidates := make([]string, 0, 2)
	for _, candidate := range []string{requestedUsername, configuredUsername} {
		if candidate == "" || containsExact(candidates, candidate) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	for _, candidate := range candidates {
		for _, user := range users {
			if user.Name == candidate {
				return user.ID
			}
		}
	}
	for _, user := range users {
		if user.Policy.IsAdministrator {
			return user.ID
		}
	}
	return ""
}

// GetItemInfo reads one user-scoped item. found is true only when the 2xx body
// is a JSON object that can be formatted as the current Emby Item subset. A
// 404 is an ordinary cache miss; other failures retain their sanitized class.
func (c *Client) GetItemInfo(ctx context.Context, userID, itemID string) (Item, bool, error) {
	userID, validUser := normalizeEmbyPathID(userID)
	itemID, validItem := normalizeEmbyPathID(itemID)
	if !validUser || !validItem {
		return Item{}, false, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return Item{}, false, err
	}
	defer cancel()

	endpoint := c.endpoint("/Users/" + url.PathEscape(userID) + "/Items/" + url.PathEscape(itemID))
	body, found, err := c.getOptional(ctx, endpoint, maxItemDetailBytes)
	if err != nil || !found {
		return Item{}, found, err
	}
	item, ok := decodeV2FormattedItem(body)
	if !ok {
		return Item{}, false, newError(ErrorCodeInvalidResponse, 0, nil)
	}
	return item, true, nil
}

// SelfUserID 是 Emby 的「当前令牌用户」别名。非管理员令牌无法枚举用户列表，
// 也就拿不到真实用户 ID；此时调度层退回该别名，条目枚举改走 /Items。
const SelfUserID = "Me"

// ListUserItems returns one bounded page from V2's user-scoped parent-folder
// endpoint. Pagination validation happens before any network request.
func (c *Client) ListUserItems(ctx context.Context, options UserItemsOptions) (ItemsResponse, error) {
	userID, validUser := normalizeEmbyPathID(options.UserID)
	parentID, validParent := normalizeEmbyPathID(options.ParentID)
	limit := options.Limit
	if limit == 0 {
		limit = maxUserItemsPageLimit
	}
	selfScoped := strings.EqualFold(strings.TrimSpace(options.UserID), SelfUserID)
	if (!validUser && !selfScoped) || !validParent || options.StartIndex < 0 || options.StartIndex > maxUserItemsStartIndex ||
		limit < 1 || limit > maxUserItemsPageLimit {
		return ItemsResponse{}, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return ItemsResponse{}, err
	}
	defer cancel()

	query := url.Values{
		"ParentId":   {parentID},
		"Fields":     {v2LibraryItemFields},
		"StartIndex": {strconv.Itoa(options.StartIndex)},
		"Limit":      {strconv.Itoa(limit)},
	}
	var result ItemsResponse
	endpoint := c.endpoint("/Users/" + url.PathEscape(userID) + "/Items")
	if selfScoped {
		// /Users/Me/Items 在部分 Emby 分支（如 fnOS 4.8）返回 500，而不带用户段的
		// /Items 会按令牌自身的可见范围返回同一批条目。
		endpoint = c.endpoint("/Items")
	}
	if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), maxUserItemsPageBytes, &result); err != nil {
		return ItemsResponse{}, err
	}
	return result, nil
}

// UserView is one per-user library view returned by V2's user-scoped library
// list (`/Users/{userID}/Views`). The sync task uses the item ID as the parent
// folder it enumerates; the collection type only informs diagnostics.
type UserView struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	CollectionType string `json:"CollectionType"`
	ServerID       string `json:"ServerId"`
}

// ListUserViews returns the bounded per-user library list used by V2's
// user-scoped media server sync. A missing or non-list `Items` value is an
// empty list, which the caller must treat as "nothing to sync" rather than as
// permission to clear that server's cache.
func (c *Client) ListUserViews(ctx context.Context, userID string) ([]UserView, error) {
	userID, validUser := normalizeEmbyPathID(userID)
	if !validUser {
		return nil, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	ctx, cancel, err := c.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	var response struct {
		Items []UserView `json:"Items"`
	}
	endpoint := c.endpoint("/Users/" + url.PathEscape(userID) + "/Views")
	if err := c.getJSON(ctx, endpoint, maxUserViewsBytes, &response); err != nil {
		return nil, err
	}
	if response.Items == nil {
		response.Items = []UserView{}
	}
	return response.Items, nil
}

func (c *Client) getOptional(ctx context.Context, endpoint string, maxBodyBytes int64) ([]byte, bool, error) {
	for attempt := 0; attempt < maxGETAttempts; attempt++ {
		response, _, err := c.doOnce(ctx, http.MethodGet, endpoint, maxBodyBytes)
		if err == nil && response.statusCode == http.StatusOK {
			return response.body, true, nil
		}
		if err != nil {
			if !shouldRetryTransport(err, attempt) {
				return nil, false, err
			}
			if waitErr := waitForRetry(ctx, retryDelay(attempt, "")); waitErr != nil {
				return nil, false, waitErr
			}
			continue
		}
		if response.statusCode == http.StatusNotFound {
			return nil, false, nil
		}
		if response.statusCode > http.StatusOK && response.statusCode < http.StatusMultipleChoices {
			return nil, false, nil
		}
		if !retryableStatus(response.statusCode) || attempt+1 == maxGETAttempts {
			return nil, false, statusError(response.statusCode)
		}
		if waitErr := waitForRetry(ctx, retryDelay(attempt, response.retryAfter)); waitErr != nil {
			return nil, false, waitErr
		}
	}
	return nil, false, newError(ErrorCodeTransport, 0, nil)
}

func decodeV2FormattedItem(body []byte) (Item, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return Item{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || !v2FormatterFieldsValid(fields) {
		return Item{}, false
	}
	var item Item
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return Item{}, false
	}
	return item, true
}

// v2FormatterFieldsValid covers response shapes that V2's formatter actively
// dereferences. Other item fields are left to Item's JSON decoder.
func v2FormatterFieldsValid(fields map[string]json.RawMessage) bool {
	if providerIDs, ok := fields["ProviderIds"]; ok {
		var values map[string]json.RawMessage
		if bytes.Equal(bytes.TrimSpace(providerIDs), []byte("null")) || json.Unmarshal(providerIDs, &values) != nil {
			return false
		}
		if tmdbID, ok := values["Tmdb"]; ok && !jsonValueIsFalsy(tmdbID) {
			var value string
			if json.Unmarshal(tmdbID, &value) != nil {
				return false
			}
			if _, err := strconv.Atoi(value); err != nil {
				return false
			}
		}
	}
	userData, ok := fields["UserData"]
	if !ok || jsonValueIsFalsy(userData) {
		return true
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(userData, &values) != nil {
		return false
	}
	if position, ok := values["PlaybackPositionTicks"]; ok && !jsonValueIsFalsy(position) {
		var value float64
		if json.Unmarshal(position, &value) != nil {
			return false
		}
	}
	if played, ok := values["Played"]; ok && !validOptionalV2Bool(played) {
		return false
	}
	if playCount, ok := values["PlayCount"]; ok && !validOptionalV2Int(playCount) {
		return false
	}
	if percentage, ok := values["PlayedPercentage"]; ok && !validOptionalV2Float(percentage) {
		return false
	}
	lastPlayed, ok := values["LastPlayedDate"]
	if !ok || bytes.Equal(bytes.TrimSpace(lastPlayed), []byte("null")) {
		return true
	}
	var value string
	if json.Unmarshal(lastPlayed, &value) != nil {
		return false
	}
	if value == "" {
		return true
	}
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		value = value[:dot]
	}
	_, err := time.Parse("2006-01-02T15:04:05", value)
	return err == nil
}

func validOptionalV2Bool(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var value bool
	if json.Unmarshal(trimmed, &value) == nil {
		return true
	}
	var number float64
	if json.Unmarshal(trimmed, &number) == nil {
		return number == 0 || number == 1
	}
	var text string
	if json.Unmarshal(trimmed, &text) != nil {
		return false
	}
	switch strings.ToLower(text) {
	case "0", "1", "off", "on", "false", "true", "no", "yes":
		return true
	default:
		return false
	}
}

func validOptionalV2Int(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var number json.Number
	if json.Unmarshal(trimmed, &number) == nil {
		if _, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
			return true
		}
		if value, err := strconv.ParseFloat(number.String(), 64); err == nil && value == math.Trunc(value) {
			return true
		}
	}
	var text string
	if json.Unmarshal(trimmed, &text) != nil {
		return false
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

func validOptionalV2Float(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var number float64
	if json.Unmarshal(trimmed, &number) == nil {
		return true
	}
	var text string
	if json.Unmarshal(trimmed, &text) != nil {
		return false
	}
	_, err := strconv.ParseFloat(text, 64)
	return err == nil
}

func jsonValueIsFalsy(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	if bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) ||
		bytes.Equal(trimmed, []byte("0")) || bytes.Equal(trimmed, []byte(`""`)) ||
		bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("{}")) {
		return true
	}
	return false
}

func normalizeEmbyPathID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != "" && len(value) <= maxEmbyPathIDBytes && value != "." && value != ".." &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
