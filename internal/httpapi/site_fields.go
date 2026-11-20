package httpapi

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func inferSiteAdapter(rawURL, name string) string {
	host := strings.ToLower(inferSiteName(rawURL))
	label := strings.ToLower(strings.TrimSpace(name))
	for _, value := range []string{host, label} {
		switch {
		case strings.Contains(value, "tnode"):
			return "TNodeSpider"
		case strings.Contains(value, "torrentleech"):
			return "TorrentLeech"
		case strings.Contains(value, "mtorrent"), strings.Contains(value, "m-team"), strings.Contains(value, "mteam"), strings.Contains(value, "馒头"):
			return "mTorrent"
		case strings.Contains(value, "sunnypt"):
			return "SunnyPT"
		case strings.Contains(value, "yema"), strings.Contains(value, "叶子"):
			return "Yema"
		case strings.Contains(value, "haidan"), strings.Contains(value, "海胆"):
			return "Haidan"
		case strings.Contains(value, "hddolby"):
			return "HDDolby"
		case strings.Contains(value, "rousi"):
			return "RousiPro"
		}
	}
	return ""
}

func inferSiteName(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Hostname())
}

func normalizedSiteDomain(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
}

func configValue(config map[string]any, key string) (any, bool) {
	if config == nil {
		return nil, false
	}
	if value, ok := config[key]; ok {
		return value, true
	}
	for existing, value := range config {
		if strings.EqualFold(strings.TrimSpace(existing), key) {
			return value, true
		}
	}
	return nil, false
}

func configString(config map[string]any, key string) string {
	value, ok := configValue(config, key)
	if !ok {
		return ""
	}
	stringValue, _ := value.(string)
	return strings.TrimSpace(stringValue)
}

func configInt(config map[string]any, key string, fallback int) int {
	value, ok := configValue(config, key)
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1
		}
		return 0
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func configOrNil(config map[string]any, key string) any {
	value, ok := configValue(config, key)
	if !ok {
		return nil
	}
	return value
}

func sensitiveSiteInputKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "cookie", "headers", "apikey", "api_key", "passkey", "token", "secret", "authorization", "_site_icon":
		return true
	default:
		return false
	}
}
