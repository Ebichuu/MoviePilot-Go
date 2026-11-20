// Package downloadflow 连接持久下载意图、站点解析器、River 与下载器对账。
package downloadflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const maxResourceLocatorLength = 1024

var (
	// ErrInvalidSourceReference 表示下载来源缺少稳定、可验证的安全身份。
	ErrInvalidSourceReference = errors.New("下载来源引用无效")
	// ErrSourceReferenceIntegrity 表示解密后的定位信息与稳定摘要不一致。
	ErrSourceReferenceIntegrity = errors.New("下载来源引用完整性校验失败")
)

// SourceReference 是生成下载候选关联键所需的非敏感字段。
type SourceReference struct {
	SiteID          int64
	ResolverKind    string
	ResolverVersion int16
	ResourceID      string
	DownloaderID    string
}

// Candidate 从不可变来源计划生成仅在内存中存在的确定性候选。
func (r SourceReference) Candidate(savePath string, paused bool) (downloader.Candidate, error) {
	r.ResolverKind = strings.ToLower(strings.TrimSpace(r.ResolverKind))
	r.ResourceID = strings.TrimSpace(r.ResourceID)
	r.DownloaderID = strings.ToLower(strings.TrimSpace(r.DownloaderID))
	if r.SiteID <= 0 || r.ResolverKind == "" || r.ResolverVersion <= 0 || r.ResourceID == "" {
		return downloader.Candidate{}, ErrInvalidSourceReference
	}
	if len(r.ResolverKind) > 64 || len(r.ResourceID) > 128 || len(savePath) > 4096 {
		return downloader.Candidate{}, ErrInvalidSourceReference
	}
	downloaderID, err := uuid.Parse(r.DownloaderID)
	if err != nil || downloaderID == uuid.Nil {
		return downloader.Candidate{}, ErrInvalidSourceReference
	}
	key := fmt.Sprintf(
		"moviepilot:source:v1:%d:%s:%d:%s:%s",
		r.SiteID,
		r.ResolverKind,
		r.ResolverVersion,
		r.ResourceID,
		downloaderID.String(),
	)
	return downloader.Candidate{Key: key, SavePath: savePath, Paused: paused}, nil
}

// ProtectResourceLocator 将纯数字资源 ID 保持为明文；其他定位信息只返回摘要和待加密秘密。
func ProtectResourceLocator(locator string) (resourceID string, resourceSecret string, err error) {
	locator = strings.TrimSpace(locator)
	if locator == "" || len(locator) > maxResourceLocatorLength || strings.ContainsAny(locator, "\r\n") {
		return "", "", ErrInvalidSourceReference
	}
	if numericResourceID(locator) {
		return locator, "", nil
	}
	digest := sha256.Sum256([]byte("moviepilot:resource:v1\x00" + locator))
	return "sha256:" + hex.EncodeToString(digest[:]), locator, nil
}

// RestoreResourceLocator 校验解密后的秘密并恢复站点解析器需要的原始定位信息。
func RestoreResourceLocator(resourceID, resourceSecret string) (string, error) {
	resourceID = strings.TrimSpace(resourceID)
	resourceSecret = strings.TrimSpace(resourceSecret)
	if resourceID == "" || len(resourceID) > 128 {
		return "", ErrInvalidSourceReference
	}
	if resourceSecret == "" {
		if !numericResourceID(resourceID) {
			return "", ErrInvalidSourceReference
		}
		return resourceID, nil
	}
	if len(resourceSecret) > maxResourceLocatorLength || strings.ContainsAny(resourceSecret, "\r\n") {
		return "", ErrInvalidSourceReference
	}
	digest := sha256.Sum256([]byte("moviepilot:resource:v1\x00" + resourceSecret))
	want := "sha256:" + hex.EncodeToString(digest[:])
	if resourceID != want {
		return "", ErrSourceReferenceIntegrity
	}
	return resourceSecret, nil
}

func numericResourceID(value string) bool {
	if len(value) == 0 || len(value) > 20 {
		return false
	}
	if value[0] == '0' && len(value) > 1 {
		return false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}
