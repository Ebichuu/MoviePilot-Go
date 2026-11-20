package cookiecloud

import "testing"

func TestBlacklistedMatchesAnyCandidate(t *testing.T) {
	indexer := Indexer{Domain: "https://indexer.example", ExtDomains: []string{"https://ext.example"}}

	if blacklisted(nil, indexer, "example") {
		t.Fatal("空黑名单不应命中")
	}
	if !blacklisted([]string{"example.com"}, indexer, "example.com") {
		t.Fatal("域名本体应命中黑名单")
	}
	if !blacklisted([]string{"https://indexer.example"}, indexer, "other.com") {
		t.Fatal("索引器 domain 应参与匹配")
	}
	if !blacklisted([]string{"https://ext.example"}, indexer, "other.com") {
		t.Fatal("扩展域名应参与匹配")
	}
	if !blacklisted([]string{"  example.com  "}, indexer, "example.com") {
		t.Fatal("黑名单条目应去空格后比较")
	}
	if blacklisted([]string{"", "   "}, indexer, "example.com") {
		t.Fatal("空白条目不应命中任何域名")
	}
	if blacklisted([]string{"not-listed"}, indexer, "example.com") {
		t.Fatal("不相关条目不应命中")
	}
}

func TestResolveIndexerDomainPrefersIndexerHost(t *testing.T) {
	indexer := Indexer{Domain: "https://www.indexer.example", ExtDomains: []string{"https://ext.example.net"}}

	mainDomain := URLDomain(indexer.Domain)
	if got := resolveIndexerDomain(indexer, mainDomain); got != indexer.Domain {
		t.Fatalf("主域名应返回索引器原值，实际 %q", got)
	}
	extDomain := URLDomain(indexer.ExtDomains[0])
	if got := resolveIndexerDomain(indexer, extDomain); got != indexer.ExtDomains[0] {
		t.Fatalf("扩展域名应返回扩展原值，实际 %q", got)
	}
	if got := resolveIndexerDomain(indexer, "unknown.example"); got != "unknown.example" {
		t.Fatalf("不匹配时应原样返回，实际 %q", got)
	}
	// 匹配是按「可注册域名」做的，所以同一主域下的子域会被判给主域名：
	// ext.indexer.example 与 www.indexer.example 的可注册域名相同。
	sameParent := Indexer{Domain: "https://www.indexer.example", ExtDomains: []string{"https://ext.indexer.example"}}
	if got := resolveIndexerDomain(sameParent, "indexer.example"); got != sameParent.Domain {
		t.Fatalf("同主域的子域应解析到主域名，实际 %q", got)
	}
}
