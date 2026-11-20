package indexer

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
)

// SiteDirectoryEntry is the maintained site.config metadata used by site
// compatibility APIs and CookieCloud's known-site lookup.
type SiteDirectoryEntry struct {
	Name       string
	Domain     string
	Adapter    string
	ExtDomains []string
	Public     bool
	Category   any
	Config     map[string]any
}

// SiteDirectory indexes only supplied site metadata; it does not invent site
// definitions for unrecognized CookieCloud domains.
type SiteDirectory struct {
	byDomain map[string]SiteDirectoryEntry
}

// NewSiteDirectory builds a domain lookup from maintained site definitions.
func NewSiteDirectory(entries []SiteDirectoryEntry) *SiteDirectory {
	directory := &SiteDirectory{byDomain: make(map[string]SiteDirectoryEntry)}
	for _, entry := range entries {
		for _, domain := range append([]string{entry.Domain}, entry.ExtDomains...) {
			for _, key := range siteDirectoryDomainKeys(domain) {
				if _, exists := directory.byDomain[key]; !exists {
					directory.byDomain[key] = entry
				}
			}
		}
	}
	return directory
}

// SiteDirectoryEntryFromConfig reads the V2 site.config metadata fields.
func SiteDirectoryEntryFromConfig(name, storedDomain, storedAdapter string, config map[string]any) SiteDirectoryEntry {
	domain := directoryConfigString(config, "domain")
	if domain == "" {
		domain = storedDomain
	}
	adapter := directoryConfigString(config, "adapter")
	if adapter == "" {
		adapter = directoryConfigString(config, "parser")
	}
	if adapter == "" {
		adapter = strings.TrimSpace(storedAdapter)
	}
	category, _ := directoryConfigValue(config, "category")
	entry := SiteDirectoryEntry{
		Name: strings.TrimSpace(name), Domain: domain, Adapter: adapter,
		ExtDomains: directoryStringList(directoryConfig(config, "ext_domains")),
		Public:     directoryBool(directoryConfig(config, "public")),
		Category:   category, Config: cloneDirectoryConfig(config),
	}
	return entry
}

// Lookup returns the maintained entry for a CookieCloud or site domain.
func (d *SiteDirectory) Lookup(domain string) (SiteDirectoryEntry, bool) {
	if d == nil {
		return SiteDirectoryEntry{}, false
	}
	for _, key := range siteDirectoryDomainKeys(domain) {
		if entry, ok := d.byDomain[key]; ok {
			return cloneDirectoryEntry(entry), true
		}
	}
	return SiteDirectoryEntry{}, false
}

// SiteCategories implements the V2 site-category lookup contract.
func (d *SiteDirectory) SiteCategories(
	ctx context.Context,
	domain string,
) ([]map[string]any, bool, error) {
	if ctx == nil {
		return []map[string]any{}, false, errors.New("站点目录请求上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return []map[string]any{}, false, err
	}
	entry, found := d.Lookup(domain)
	if !found {
		return []map[string]any{}, false, nil
	}
	return flattenSiteCategories(entry.Category), true, nil
}

func flattenSiteCategories(value any) []map[string]any {
	result := make([]map[string]any, 0)
	var visit func(any)
	visit = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				visit(item)
			}
		case []map[string]any:
			for _, item := range typed {
				visit(item)
			}
		case map[string]any:
			if isSiteCategory(typed) {
				result = append(result, cloneSiteCategory(typed))
				return
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				visit(typed[key])
			}
		case map[string]string:
			converted := make(map[string]any, len(typed))
			for key, item := range typed {
				converted[key] = item
			}
			result = append(result, converted)
		}
	}
	visit(value)
	if result == nil {
		return []map[string]any{}
	}
	unique := make([]map[string]any, 0, len(result))
	seen := make(map[string]struct{}, len(result))
	for _, item := range result {
		key := siteCategoryIdentity(item)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, item)
	}
	return unique
}

func isSiteCategory(value map[string]any) bool {
	for key := range value {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "id", "name", "cat", "category", "value":
			return true
		}
	}
	return false
}

func siteCategoryIdentity(category map[string]any) string {
	for _, key := range []string{"id", "cat", "value", "name"} {
		if value, exists := directoryConfigValue(category, key); exists {
			return key + "\x00" + strings.TrimSpace(directoryString(value))
		}
	}
	keys := make([]string, 0, len(category))
	for key := range category {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var identity strings.Builder
	for _, key := range keys {
		identity.WriteString(key)
		identity.WriteByte('=')
		identity.WriteString(directoryString(category[key]))
		identity.WriteByte(0)
	}
	return identity.String()
}

func cloneSiteCategory(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneDirectoryEntry(entry SiteDirectoryEntry) SiteDirectoryEntry {
	entry.ExtDomains = append([]string(nil), entry.ExtDomains...)
	entry.Config = cloneDirectoryConfig(entry.Config)
	return entry
}

func cloneDirectoryConfig(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func directoryConfig(config map[string]any, key string) any {
	value, _ := directoryConfigValue(config, key)
	return value
}

func directoryConfigValue(config map[string]any, key string) (any, bool) {
	for existing, value := range config {
		if strings.EqualFold(strings.TrimSpace(existing), key) {
			return value, true
		}
	}
	return nil, false
}

func directoryConfigString(config map[string]any, key string) string {
	value, _ := directoryConfigValue(config, key)
	return strings.TrimSpace(directoryString(value))
}

func directoryString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

func directoryStringList(value any) []string {
	var values []string
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(directoryString(item)); text != "" {
				values = append(values, text)
			}
		}
	case string:
		values = append(values, strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == ';' })...)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func directoryBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
	default:
		return false
	}
}

func siteDirectoryDomainKeys(raw string) []string {
	host := strings.TrimSpace(strings.ToLower(raw))
	if host == "" {
		return nil
	}
	if !strings.Contains(host, "://") {
		host = "https://" + strings.TrimLeft(host, ".")
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	keys := []string{hostname}
	labels := strings.Split(hostname, ".")
	if len(labels) > 3 {
		keys = append(keys, strings.Join(labels[len(labels)-2:], "."))
	} else if len(labels) > 2 {
		keys = append(keys, strings.Join(labels[len(labels)-2:], "."))
	}
	unique := keys[:0]
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique
}
