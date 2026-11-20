package metadata

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Raw 直连 Bangumi v0 的某个路径，返回原始 JSON，用于 V2 的透传型接口。
func (c *Bangumi) Raw(ctx context.Context, path string, query url.Values) (any, error) {
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, errors.New("Bangumi 路径不能为空")
	}
	if query == nil {
		query = url.Values{}
	}
	endpoint := "/" + path
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	var result any
	if err := c.do(ctx, "GET", endpoint, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Raw 执行一条 AniList GraphQL 查询并返回 data 字段，用于 V2 的透传型接口。
func (c *AniList) Raw(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := c.do(ctx, query, variables, &response); err != nil {
		return nil, err
	}
	if response.Data == nil {
		response.Data = map[string]any{}
	}
	return response.Data, nil
}
