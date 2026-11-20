package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errSiteUserDataUnsupported = errors.New("站点用户数据解析器不存在")

type siteUserDataOptionsContextKey struct{}

// SiteUserDataOptions controls V2 parser behavior that is configured globally.
type SiteUserDataOptions struct {
	FetchMessages bool
}

// SiteUserData is the parser-owned projection returned by V2 site refreshes.
type SiteUserData struct {
	Domain                string    `json:"domain"`
	Username              *string   `json:"username"`
	UserID                any       `json:"userid"`
	UserLevel             *string   `json:"user_level"`
	JoinAt                *string   `json:"join_at"`
	Bonus                 float64   `json:"bonus"`
	Upload                int64     `json:"upload"`
	Download              int64     `json:"download"`
	Ratio                 float64   `json:"ratio"`
	Seeding               int64     `json:"seeding"`
	Leeching              int64     `json:"leeching"`
	SeedingSize           int64     `json:"seeding_size"`
	LeechingSize          int64     `json:"leeching_size"`
	SeedingInfo           [][]int64 `json:"seeding_info"`
	MessageUnread         int       `json:"message_unread"`
	MessageUnreadContents [][]any   `json:"message_unread_contents"`
	ErrMsg                *string   `json:"err_msg"`
	UpdatedDay            string    `json:"updated_day"`
	UpdatedTime           *string   `json:"updated_time"`
}

// SiteUserDataFetcher is the optional capability implemented by V2 userdata parsers.
type SiteUserDataFetcher interface {
	FetchSiteUserData(context.Context) (SiteUserData, error)
}

// FetchSiteUserData runs one refresh under the same per-site gate used by search and probes.
func (c *Coordinator) FetchSiteUserData(
	ctx context.Context,
	adapter Adapter,
	options ...SiteUserDataOptions,
) (SiteUserData, error) {
	if ctx == nil || c == nil || adapter == nil {
		return SiteUserData{}, errSiteUserDataUnsupported
	}
	fetcher, ok := adapter.(SiteUserDataFetcher)
	if !ok {
		return SiteUserData{}, errSiteUserDataUnsupported
	}
	if err := ctx.Err(); err != nil {
		return SiteUserData{}, err
	}
	gate := c.siteGate(adapterGateKey(adapter))
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return SiteUserData{}, ctx.Err()
	}
	fetchMessages := true
	if len(options) > 0 {
		fetchMessages = options[0].FetchMessages
	}
	ctx = context.WithValue(ctx, siteUserDataOptionsContextKey{}, SiteUserDataOptions{
		FetchMessages: fetchMessages,
	})
	return fetcher.FetchSiteUserData(ctx)
}

func siteUserDataMessagesEnabled(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	options, ok := ctx.Value(siteUserDataOptionsContextKey{}).(SiteUserDataOptions)
	return !ok || options.FetchMessages
}

func newSiteUserData(base *url.URL) SiteUserData {
	return SiteUserData{
		Domain: siteUserDataDomain(base), SeedingInfo: make([][]int64, 0),
		MessageUnreadContents: make([][]any, 0), UpdatedDay: time.Now().Format("2006-01-02"),
	}
}

func siteUserDataDomain(base *url.URL) string {
	if base == nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	for _, special := range []string{"u2.dmhy.org", "pt.ecust.pp.ua", "pt.gtkpw.xyz", "pt.gtk.pw"} {
		if strings.Contains(host, special) {
			return special
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) > 3 {
		return host
	}
	if len(labels) >= 2 {
		return strings.Join(labels[len(labels)-2:], ".")
	}
	return host
}

func userDataString(value any) *string {
	if value == nil {
		return nil
	}
	text := valueString(value)
	return &text
}

func userDataInt64(value any) int64 {
	text := strings.ReplaceAll(strings.TrimSpace(valueString(value)), ",", "")
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		return max(parsed, 0)
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > math.MaxInt64 {
		return 0
	}
	return int64(number)
}

func userDataFloat(value any) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(valueString(value)), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0
	}
	return parsed
}

func userDataInt(value any) int {
	parsed := userDataInt64(value)
	if uint64(parsed) > uint64(^uint(0)>>1) {
		return 0
	}
	return int(parsed)
}

func userDataID(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
		return typed
	case float64:
		if typed >= math.MinInt64 && typed <= math.MaxInt64 && typed == math.Trunc(typed) {
			return int64(typed)
		}
	}
	return value
}

func userDataDateTime(value any) *string {
	if value == nil {
		return nil
	}
	raw := strings.TrimSpace(valueString(value))
	if raw == "" {
		return userDataString("")
	}
	if timestamp, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if timestamp > 1_000_000_000_000 {
			timestamp /= 1000
		}
		if timestamp > 0 {
			formatted := time.Unix(timestamp, 0).Format("2006-01-02 15:04:05")
			return &formatted
		}
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339, time.RFC1123Z, time.RFC1123,
		"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
	} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			formatted := parsed.Format("2006-01-02 15:04:05")
			return &formatted
		}
	}
	return &raw
}
