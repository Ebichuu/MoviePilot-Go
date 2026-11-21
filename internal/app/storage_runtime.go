package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/storage"
	"github.com/moviepilot-go/moviepilot-go/internal/subtitle"
)

const storagesSettingKey = "Storages"

type storageSettingStore interface {
	GetSetting(ctx context.Context, key string, target any) (bool, error)
	SetSetting(ctx context.Context, key string, value any, encrypted bool) error
}

type persistedStorageEntry struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

type storageSettingPersister struct {
	mu    sync.Mutex
	store storageSettingStore
}

func (p *storageSettingPersister) loadConfig(ctx context.Context, storageType string) map[string]any {
	if p == nil || p.store == nil {
		return nil
	}
	var entries []persistedStorageEntry
	found, err := p.store.GetSetting(ctx, storagesSettingKey, &entries)
	if err != nil || !found {
		return nil
	}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.Type), storageType) {
			return entry.Config
		}
	}
	return nil
}

func (p *storageSettingPersister) saveCallback(storageType, displayName string) func(context.Context, map[string]any) error {
	if p == nil || p.store == nil {
		return nil
	}
	return func(ctx context.Context, conf map[string]any) error {
		p.mu.Lock()
		defer p.mu.Unlock()

		var entries []persistedStorageEntry
		_, _ = p.store.GetSetting(ctx, storagesSettingKey, &entries)

		updated := false
		for i := range entries {
			if strings.EqualFold(strings.TrimSpace(entries[i].Type), storageType) {
				entries[i].Config = conf
				if strings.TrimSpace(entries[i].Name) == "" {
					entries[i].Name = displayName
				}
				updated = true
				break
			}
		}
		if !updated {
			entries = append(entries, persistedStorageEntry{
				Type:   storageType,
				Name:   displayName,
				Config: conf,
			})
		}
		return p.store.SetSetting(ctx, storagesSettingKey, entries, true)
	}
}

func buildStorageProvider(
	ctx context.Context,
	cfg config.Config,
	store storageSettingStore,
	logger *slog.Logger,
) storage.Provider {
	registry := storage.NewProviderRegistry()
	persister := &storageSettingPersister{store: store}

	if storageRoot := strings.TrimSpace(cfg.StorageLocalPath); storageRoot != "" {
		registry.Register("local", storage.NewLocalProvider(map[string]string{"local": storageRoot}))
	}
	if rcloneURL := strings.TrimSpace(cfg.StorageRcloneURL); rcloneURL != "" {
		if rclone, err := storage.NewRcloneProvider(storage.RcloneConfig{
			URL:        rcloneURL,
			FileSystem: cfg.StorageRcloneFS,
			Username:   cfg.StorageRcloneUsername,
			Password:   cfg.StorageRclonePassword,
		}); err == nil {
			registry.Register(rclone.Name(), rclone)
		} else if logger != nil {
			logger.Warn("初始化 Rclone 存储失败", "error", err)
		}
	}
	if alistURL := strings.TrimSpace(cfg.StorageAlistURL); alistURL != "" {
		if alist, err := storage.NewAlistProvider(storage.AlistConfig{
			URL:      alistURL,
			Username: cfg.StorageAlistUsername,
			Password: cfg.StorageAlistPassword,
			Root:     cfg.StorageAlistRoot,
		}); err == nil {
			registry.Register(alist.Name(), alist)
		} else if logger != nil {
			logger.Warn("初始化 Alist 存储失败", "error", err)
		}
	}

	smbCfg := storage.SMBConfig{
		Host:           cfg.StorageSMBHost,
		Port:           cfg.StorageSMBPort,
		Share:          cfg.StorageSMBShare,
		Username:       cfg.StorageSMBUsername,
		Password:       cfg.StorageSMBPassword,
		Domain:         cfg.StorageSMBDomain,
		Root:           cfg.StorageSMBRoot,
		Timeout:        cfg.RequestTimeout,
		OnConfigChange: persister.saveCallback("smb", "SMB"),
	}
	if saved := persister.loadConfig(ctx, "smb"); len(saved) > 0 {
		applySavedSMBConfig(&smbCfg, saved)
	}
	if smb, err := storage.NewSMBProvider(smbCfg); err == nil {
		registry.Register(smb.Name(), smb)
	}

	alipanCfg := storage.AlipanConfig{
		AppID:          cfg.StorageAlipanAppID,
		AccessToken:    cfg.StorageAlipanAccessToken,
		RefreshToken:   cfg.StorageAlipanRefreshToken,
		DriveID:        cfg.StorageAlipanDriveID,
		Timeout:        cfg.RequestTimeout,
		OnConfigChange: persister.saveCallback("alipan", "阿里云盘"),
	}
	if saved := persister.loadConfig(ctx, "alipan"); len(saved) > 0 {
		applySavedAlipanConfig(&alipanCfg, saved)
	}
	if alipan, err := storage.NewAlipanProvider(alipanCfg); err == nil {
		registry.Register(alipan.Name(), alipan)
	}

	u115Cfg := storage.U115Config{
		AppID:          cfg.StorageU115AppID,
		AuthServerURL:  cfg.StorageU115AuthServer,
		AccessToken:    cfg.StorageU115AccessToken,
		RefreshToken:   cfg.StorageU115RefreshToken,
		Timeout:        cfg.RequestTimeout,
		OnConfigChange: persister.saveCallback("u115", "115网盘"),
	}
	if saved := persister.loadConfig(ctx, "u115"); len(saved) > 0 {
		applySavedU115Config(&u115Cfg, saved)
	}
	if u115, err := storage.NewU115Provider(u115Cfg); err == nil {
		registry.Register(u115.Name(), u115)
	}

	return registry
}

func buildSubtitleCoordinator(cfg config.Config, logger *slog.Logger) *subtitle.Coordinator {
	client := &http.Client{Timeout: cfg.RequestTimeout}
	var providers []subtitle.Provider
	if apiKey := strings.TrimSpace(cfg.OpenSubtitlesAPIKey); apiKey != "" {
		p, err := subtitle.NewOpenSubtitlesProvider(subtitle.OpenSubtitlesConfig{
			APIKey: apiKey,
			Token:  cfg.OpenSubtitlesToken,
			Client: client,
		})
		if err == nil {
			providers = append(providers, p)
		} else if logger != nil {
			logger.Warn("初始化 OpenSubtitles 字幕源失败", "error", err)
		}
	}
	if token := strings.TrimSpace(cfg.AssrtToken); token != "" {
		p, err := subtitle.NewAssrtProvider(subtitle.AssrtConfig{
			Token:  token,
			Client: client,
		})
		if err == nil {
			providers = append(providers, p)
		} else if logger != nil {
			logger.Warn("初始化 Assrt 字幕源失败", "error", err)
		}
	}
	return subtitle.NewCoordinator(client, cfg.SubtitleLanguages, providers...)
}

func applySavedSMBConfig(dst *storage.SMBConfig, saved map[string]any) {
	if dst.Host == "" {
		dst.Host = mapStr(saved, "host")
	}
	if dst.Share == "" {
		dst.Share = mapStr(saved, "share")
	}
	if dst.Username == "" {
		dst.Username = mapStr(saved, "username")
	}
	if dst.Password == "" {
		dst.Password = mapStr(saved, "password")
	}
	if dst.Domain == "" {
		dst.Domain = mapStr(saved, "domain")
	}
	if dst.Root == "" {
		dst.Root = mapStr(saved, "root")
	}
	if port := mapInt(saved, "port"); port > 0 && dst.Port == 445 {
		dst.Port = port
	}
}

func applySavedAlipanConfig(dst *storage.AlipanConfig, saved map[string]any) {
	if dst.AccessToken == "" {
		dst.AccessToken = mapStr(saved, "access_token")
	}
	if dst.RefreshToken == "" {
		dst.RefreshToken = mapStr(saved, "refresh_token")
	}
	if dst.DriveID == "" {
		dst.DriveID = mapStr(saved, "drive_id")
	}
	if dst.ResourceDriveID == "" {
		dst.ResourceDriveID = mapStr(saved, "resource_drive_id")
	}
	if dst.BackupDriveID == "" {
		dst.BackupDriveID = mapStr(saved, "backup_drive_id")
	}
	if dst.DefaultDriveID == "" {
		dst.DefaultDriveID = mapStr(saved, "default_drive_id")
	}
	if dst.ExpiresIn == 0 {
		dst.ExpiresIn = mapInt64(saved, "expires_in")
	}
	if dst.RefreshTime == 0 {
		dst.RefreshTime = mapInt64(saved, "refresh_time")
	}
}

func applySavedU115Config(dst *storage.U115Config, saved map[string]any) {
	if dst.AccessToken == "" {
		dst.AccessToken = mapStr(saved, "access_token")
	}
	if dst.RefreshToken == "" {
		dst.RefreshToken = mapStr(saved, "refresh_token")
	}
	if dst.ExpiresIn == 0 {
		dst.ExpiresIn = mapInt64(saved, "expires_in")
	}
	if dst.RefreshTime == 0 {
		dst.RefreshTime = mapInt64(saved, "refresh_time")
	}
}

func mapStr(m map[string]any, key string) string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func mapInt(m map[string]any, key string) int {
	return int(mapInt64(m, key))
}

func mapInt64(m map[string]any, key string) int64 {
	raw, ok := m[key]
	if !ok || raw == nil {
		return 0
	}
	switch v := raw.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}
