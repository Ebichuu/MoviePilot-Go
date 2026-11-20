package metadata

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Raw 直连 TMDB 的某个 REST 路径，返回原始 JSON 对象。
// 用于 V2 里那些「透传 TMDB 形状」的接口（collection / credits / person / recommend / similar）。
func (c *TMDB) Raw(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	if c.http == nil || c.baseURL == "" {
		return nil, errors.New("TMDB 客户端未配置")
	}
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, errors.New("TMDB 路径不能为空")
	}
	if query == nil {
		query = url.Values{}
	}
	if query.Get("language") == "" {
		query.Set("language", "zh-CN")
	}
	endpoint := c.signed(c.baseURL + "/3/" + path + "?" + query.Encode())
	result := map[string]any{}
	if err := c.http.DoJSON(ctx, "GET", endpoint, c.headers(), nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}
