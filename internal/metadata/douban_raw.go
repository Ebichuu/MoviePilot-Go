package metadata

import (
	"context"
	"net/url"
)

// Raw 直连豆瓣 API 的任意只读路径（人物、演职员、推荐等），
// 供 V2 的豆瓣附属查询接口透传使用。
func (c *Douban) Raw(ctx context.Context, path string, query url.Values) (any, error) {
	var target any
	if err := c.get(ctx, path, query, &target); err != nil {
		return nil, err
	}
	return target, nil
}
