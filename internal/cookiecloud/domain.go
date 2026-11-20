package cookiecloud

import (
	"net/url"
	"strings"
)

// specialDomains 复刻 V2 `app/utils/string.py:15–20` 的特殊域名表。
var specialDomains = []string{
	"u2.dmhy.org",
	"pt.ecust.pp.ua",
	"pt.gtkpw.xyz",
	"pt.gtk.pw",
}

// URLDomain 复刻 V2 `StringUtils.get_url_domain`（app/utils/string.py:305–320）：
// 命中特殊域名直接返回该域名；否则取 netloc，超过 3 段保留完整 netloc，否则取
// 最后两级。
func URLDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, domain := range specialDomains {
		if strings.Contains(raw, domain) {
			return domain
		}
	}
	netloc := urlNetloc(raw)
	if netloc == "" {
		return ""
	}
	labels := strings.Split(netloc, ".")
	if len(labels) > 3 {
		return netloc
	}
	if len(labels) < 2 {
		return ""
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// urlNetloc 从 cookie domain（可能带前导点、可能为完整 URL）提取主机名。
func urlNetloc(raw string) string {
	candidate := strings.TrimSpace(raw)
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + strings.TrimLeft(candidate, ".")
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
