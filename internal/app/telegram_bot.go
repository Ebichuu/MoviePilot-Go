package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/telegrambot"
)

// telegramSearchLimit 是机器人单次搜索向各站点请求的结果上限。
const telegramSearchLimit = 30

// telegramServices 是机器人可见的只读业务能力：
// 搜索、订阅列表、站点列表。新增/下载等写操作仍留在 Web 端。
type telegramServices struct {
	search   *indexer.Coordinator
	registry *indexer.Registry
	store    *database.Store
	username string
}

var _ telegrambot.Services = (*telegramServices)(nil)

func newTelegramServices(
	search *indexer.Coordinator,
	registry *indexer.Registry,
	store *database.Store,
	username string,
) *telegramServices {
	return &telegramServices{search: search, registry: registry, store: store, username: username}
}

func (s *telegramServices) SearchTitles(
	ctx context.Context,
	keyword string,
	limit int,
) ([]telegrambot.SearchResult, error) {
	if s == nil || s.search == nil || s.registry == nil {
		return nil, errors.New("搜索服务未就绪")
	}
	adapters := s.registry.Snapshot()
	if len(adapters) == 0 {
		return nil, errors.New("没有启用的站点")
	}
	if limit <= 0 || limit > telegramSearchLimit {
		limit = telegramSearchLimit
	}
	resources, err := s.search.Search(ctx, adapters, indexer.Query{
		Keyword: strings.TrimSpace(keyword),
		Page:    1,
		Limit:   limit,
	}, nil)
	if err != nil {
		return nil, err
	}
	results := make([]telegrambot.SearchResult, 0, len(resources))
	for _, resource := range resources {
		if len(results) >= limit {
			break
		}
		title := strings.TrimSpace(resource.Title)
		if title == "" {
			continue
		}
		results = append(results, telegrambot.SearchResult{
			Title:     title,
			Site:      strings.TrimSpace(resource.Site),
			Size:      resource.SizeBytes,
			Seeders:   resource.Seeders,
			DetailURL: strings.TrimSpace(resource.DetailURL),
		})
	}
	return results, nil
}

func (s *telegramServices) Subscriptions(ctx context.Context) ([]string, error) {
	if s == nil || s.store == nil || strings.TrimSpace(s.username) == "" {
		return nil, errors.New("订阅查询未就绪")
	}
	items, err := s.store.ListSubscriptions(ctx, s.username, true)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		title := strings.TrimSpace(item.Name)
		if title == "" {
			title = fmt.Sprintf("订阅 #%d", item.ID)
		}
		if year := strings.TrimSpace(item.Year); year != "" {
			title += " (" + year + ")"
		}
		parts := []string{title}
		if mediaType := strings.TrimSpace(item.MediaType); mediaType != "" {
			parts = append(parts, mediaType)
		}
		if state := strings.TrimSpace(item.V2State); state != "" {
			parts = append(parts, state)
		} else if item.InternalState != "" {
			parts = append(parts, string(item.InternalState))
		}
		lines = append(lines, "· "+strings.Join(parts, " · "))
	}
	return lines, nil
}

func (s *telegramServices) Sites(ctx context.Context) ([]string, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("站点查询未就绪")
	}
	sites, err := s.store.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(sites))
	for _, site := range sites {
		name := strings.TrimSpace(site.Name)
		if name == "" {
			name = fmt.Sprintf("站点 #%d", site.ID)
		}
		state := "已禁用"
		if site.Enabled {
			state = "已启用"
		}
		lines = append(lines, fmt.Sprintf("· %s · %s", name, state))
	}
	return lines, nil
}

// telegramOwnerUsername 选择机器人代表的操作账号：优先超级管理员。
func telegramOwnerUsername(ctx context.Context, store *database.Store) string {
	if store == nil {
		return ""
	}
	users, err := store.ListUsers(ctx)
	if err != nil {
		return ""
	}
	for _, user := range users {
		if user.SuperUser {
			return user.Username
		}
	}
	if len(users) > 0 {
		return users[0].Username
	}
	return ""
}

// startTelegramBots 启动入站轮询管理器。配置来自通知运行态的 Telegram 通道：
// 有 Token 且启用的通道各起一个 getUpdates 循环，配置变更 60 秒内生效。
func startTelegramBots(
	ctx context.Context,
	logger *slog.Logger,
	runtime *notificationRuntime,
	search *indexer.Coordinator,
	registry *indexer.Registry,
	store *database.Store,
) *telegrambot.Manager {
	if ctx == nil || logger == nil || runtime == nil {
		return nil
	}
	services := newTelegramServices(search, registry, store, telegramOwnerUsername(ctx, store))
	router := telegrambot.NewRouter(logger, services)
	manager := telegrambot.NewManager(logger, router)
	go manager.Run(ctx, func(ctx context.Context) []telegrambot.Config {
		configs := runtime.Telegrams()
		out := make([]telegrambot.Config, 0, len(configs))
		for _, config := range configs {
			out = append(out, telegrambot.Config{
				Name:    config.Name,
				Token:   config.BotToken,
				ChatID:  config.ChatID,
				APIURL:  config.APIURL,
				Enabled: true,
			})
		}
		return out
	})
	return manager
}
