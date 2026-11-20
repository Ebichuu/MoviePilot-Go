package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/joho/godotenv"
)

const (
	defaultEnvironment         = "development"
	defaultHTTPAddr            = ":3000"
	defaultDatabaseURLDev      = "postgres://moviepilot:moviepilot@127.0.0.1:5432/moviepilot?sslmode=disable"
	defaultPostgresMaxConns    = 16
	defaultRedisPoolSize       = 16
	defaultMaxCacheValue       = 4 << 20
	defaultAccessTokenLifetime = 24 * time.Hour
	defaultBootstrapUsername   = "admin"
	defaultBootstrapPassword   = "moviepilot-dev"
	defaultDevMasterKey        = "moviepilot-dev-master-key-change-me"
	defaultFrontendDir         = "web/dist"
	defaultConfigDir           = "config"
	defaultTimezone            = "Asia/Shanghai"
	defaultRequestTimeout      = 30 * time.Second
	defaultShutdownTimeout     = 30 * time.Second
	defaultMaxRequestBody      = 4 << 20
	defaultMaxResponseBody     = 8 << 20
	defaultSearchWorkers       = 8
	defaultOrganizeWorkers     = 4
	defaultScrapeWorkers       = 4
	defaultMetadataWorkers     = 12
	defaultMetadataTimeout     = 10 * time.Second
	defaultMetadataCacheTTL    = 12 * time.Hour
	defaultMetadataMinInterval = 100 * time.Millisecond
	defaultMetadataMaxBackoff  = 30 * time.Second
	defaultMetadataMaxResults  = 20
	defaultTMDBBaseURL         = "https://api.themoviedb.org"
	defaultTMDBImageDomain     = "image.tmdb.org"
	defaultDoubanBaseURL       = "https://frodo.douban.com/api/v2"
	defaultBangumiBaseURL      = "https://api.bgm.tv"
	defaultAniListEndpoint     = "https://graphql.anilist.co"
	defaultSubscribeRSS        = 30 * time.Minute
	defaultCookieCloudHost     = "https://movie-pilot.org/cookiecloud"
	defaultCookieCloudInterval = 24 * time.Hour
	defaultStorageAlistRoot    = "/"
	defaultLoginMaxFailures    = 5
	defaultLoginIPMaxFailures  = 20
	defaultLoginIPWindow       = 10 * time.Minute
	defaultLoginLockBase       = time.Minute
	defaultLoginLockMax        = 15 * time.Minute
)

// Config 保存 MoviePilot Go 的进程级运行时配置。
// 配置优先级：进程环境变量 > CONFIG_DIR/app.env（默认 config/app.env）> 内置默认值。
type Config struct {
	Environment string
	HTTPAddr    string

	DatabaseURL      string
	PostgresMaxConns int32

	RedisURL      string
	RedisPoolSize int
	MaxCacheValue int

	JWTSecret           string
	ResourceSecret      string
	MasterKey           string
	AccessTokenLifetime time.Duration
	BootstrapUsername   string
	BootstrapPassword   string
	APIToken            string

	FrontendDir string
	ConfigDir   string
	AppDomain   string
	Location    *time.Location

	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	MaxRequestBody  int64
	MaxResponseBody int64

	SearchWorkers         int
	OrganizeWorkers       int
	ScrapeWorkers         int
	AllowPrivateSiteHosts bool
	ProxyHost             string

	MetadataWorkers     int
	MetadataTimeout     time.Duration
	MetadataCacheTTL    time.Duration
	MetadataMinInterval time.Duration
	MetadataMaxBackoff  time.Duration
	MetadataMaxResults  int

	TMDBBaseURL     string
	TMDBToken       string
	TMDBImageDomain string
	DoubanBaseURL   string
	DoubanAPIKey    string
	DoubanSecretKey string
	BangumiBaseURL  string
	BangumiToken    string
	AniListEndpoint string

	SubscribeRSSInterval time.Duration
	SiteMessage          bool

	CookieCloudHost        string
	CookieCloudKey         string
	CookieCloudPassword    string
	CookieCloudEnableLocal bool
	CookieCloudCookiePath  string
	CookieCloudInterval    time.Duration
	CookieCloudBlacklist   []string

	StorageLocalPath      string
	StorageRcloneURL      string
	StorageRcloneFS       string
	StorageRcloneUsername string
	StorageRclonePassword string
	StorageAlistURL       string
	StorageAlistUsername  string
	StorageAlistPassword  string
	StorageAlistRoot      string

	VAPIDPrivateKey  string
	VAPIDPublicKey   string
	VAPIDSubject     string
	WebPushUsernames []string

	LoginMaxFailures   int
	LoginIPMaxFailures int
	LoginIPWindow      time.Duration
	LoginLockBase      time.Duration
	LoginLockMax       time.Duration
}

// Load 按「进程环境变量 > CONFIG_DIR/app.env > 内置默认值」顺序加载并校验配置。
func Load() (Config, error) {
	configDir := strings.TrimSpace(firstNonEmpty(os.Getenv("CONFIG_DIR"), os.Getenv("MOVIEPILOT_CONFIG_DIR")))
	if configDir == "" {
		configDir = defaultConfigDir
	}

	fileValues, err := loadEnvFile(filepath.Join(configDir, "app.env"))
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件 %s: %w", filepath.Join(configDir, "app.env"), err)
	}

	lookup := func(keys ...string) string {
		for _, key := range keys {
			if value := strings.TrimSpace(os.Getenv(key)); value != "" {
				return value
			}
		}
		for _, key := range keys {
			if value := strings.TrimSpace(fileValues[key]); value != "" {
				return value
			}
		}
		return ""
	}

	env := strings.ToLower(lookup("MOVIEPILOT_ENV"))
	if env == "" {
		env = defaultEnvironment
	}
	isProd := env == "production" || env == "prod"

	cfg := Config{
		Environment: env,
		HTTPAddr:    defaultString(lookup("MOVIEPILOT_HTTP_ADDR"), defaultHTTPAddr),

		DatabaseURL:      lookup("MOVIEPILOT_DATABASE_URL"),
		PostgresMaxConns: defaultPostgresMaxConns,

		RedisURL:      lookup("MOVIEPILOT_REDIS_URL"),
		RedisPoolSize: defaultRedisPoolSize,
		MaxCacheValue: defaultMaxCacheValue,

		JWTSecret:           lookup("MOVIEPILOT_JWT_SECRET"),
		ResourceSecret:      lookup("MOVIEPILOT_RESOURCE_SECRET"),
		MasterKey:           lookup("MOVIEPILOT_MASTER_KEY"),
		AccessTokenLifetime: defaultAccessTokenLifetime,
		BootstrapUsername:   defaultString(lookup("MOVIEPILOT_BOOTSTRAP_USERNAME"), defaultBootstrapUsername),
		BootstrapPassword:   lookup("MOVIEPILOT_BOOTSTRAP_PASSWORD"),
		APIToken:            lookup("MOVIEPILOT_API_TOKEN", "API_TOKEN"),

		FrontendDir: defaultString(lookup("MOVIEPILOT_FRONTEND_DIR"), defaultFrontendDir),
		ConfigDir:   configDir,
		AppDomain:   lookup("APP_DOMAIN", "MOVIEPILOT_APP_DOMAIN"),

		RequestTimeout:  defaultRequestTimeout,
		ShutdownTimeout: defaultShutdownTimeout,
		MaxRequestBody:  defaultMaxRequestBody,
		MaxResponseBody: defaultMaxResponseBody,

		SearchWorkers:         defaultSearchWorkers,
		OrganizeWorkers:       defaultOrganizeWorkers,
		ScrapeWorkers:         defaultScrapeWorkers,
		AllowPrivateSiteHosts: parseBoolDefault(lookup("ALLOW_PRIVATE_SITE_HOSTS", "MOVIEPILOT_ALLOW_PRIVATE_SITE_HOSTS"), false),
		ProxyHost:             lookup("PROXY_HOST", "MOVIEPILOT_PROXY_HOST"),

		MetadataWorkers:     defaultMetadataWorkers,
		MetadataTimeout:     defaultMetadataTimeout,
		MetadataCacheTTL:    defaultMetadataCacheTTL,
		MetadataMinInterval: defaultMetadataMinInterval,
		MetadataMaxBackoff:  defaultMetadataMaxBackoff,
		MetadataMaxResults:  defaultMetadataMaxResults,

		TMDBBaseURL:     resolveTMDBBaseURL(lookup("MOVIEPILOT_TMDB_BASE_URL"), lookup("TMDB_API_DOMAIN")),
		TMDBToken:       lookup("MOVIEPILOT_TMDB_TOKEN", "TMDB_API_KEY"),
		TMDBImageDomain: defaultString(lookup("TMDB_IMAGE_DOMAIN", "MOVIEPILOT_TMDB_IMAGE_DOMAIN"), defaultTMDBImageDomain),
		DoubanBaseURL:   defaultString(lookup("MOVIEPILOT_DOUBAN_BASE_URL"), defaultDoubanBaseURL),
		DoubanAPIKey:    lookup("MOVIEPILOT_DOUBAN_API_KEY"),
		DoubanSecretKey: lookup("MOVIEPILOT_DOUBAN_SECRET_KEY"),
		BangumiBaseURL:  defaultString(lookup("MOVIEPILOT_BANGUMI_BASE_URL"), defaultBangumiBaseURL),
		BangumiToken:    lookup("MOVIEPILOT_BANGUMI_TOKEN"),
		AniListEndpoint: defaultString(lookup("MOVIEPILOT_ANILIST_ENDPOINT"), defaultAniListEndpoint),

		SubscribeRSSInterval: defaultSubscribeRSS,
		SiteMessage:          parseBoolDefault(lookup("SITE_MESSAGE", "MOVIEPILOT_SITE_MESSAGE"), false),

		CookieCloudHost:        defaultString(lookup("COOKIECLOUD_HOST"), defaultCookieCloudHost),
		CookieCloudKey:         lookup("COOKIECLOUD_KEY"),
		CookieCloudPassword:    lookup("COOKIECLOUD_PASSWORD"),
		CookieCloudEnableLocal: parseBoolDefault(lookup("COOKIECLOUD_ENABLE_LOCAL"), false),
		CookieCloudCookiePath:  lookup("COOKIECLOUD_COOKIE_PATH"),
		CookieCloudInterval:    defaultCookieCloudInterval,
		CookieCloudBlacklist:   parseCSV(lookup("COOKIECLOUD_BLACKLIST")),

		StorageLocalPath:      lookup("MOVIEPILOT_STORAGE_LOCAL_PATH"),
		StorageRcloneURL:      lookup("MOVIEPILOT_STORAGE_RCLONE_URL"),
		StorageRcloneFS:       lookup("MOVIEPILOT_STORAGE_RCLONE_FS"),
		StorageRcloneUsername: lookup("MOVIEPILOT_STORAGE_RCLONE_USERNAME"),
		StorageRclonePassword: lookup("MOVIEPILOT_STORAGE_RCLONE_PASSWORD"),
		StorageAlistURL:       lookup("MOVIEPILOT_STORAGE_ALIST_URL"),
		StorageAlistUsername:  lookup("MOVIEPILOT_STORAGE_ALIST_USERNAME"),
		StorageAlistPassword:  lookup("MOVIEPILOT_STORAGE_ALIST_PASSWORD"),
		StorageAlistRoot:      defaultString(lookup("MOVIEPILOT_STORAGE_ALIST_ROOT"), defaultStorageAlistRoot),

		VAPIDPrivateKey:  lookup("MOVIEPILOT_VAPID_PRIVATE_KEY"),
		VAPIDPublicKey:   lookup("MOVIEPILOT_VAPID_PUBLIC_KEY"),
		VAPIDSubject:     lookup("MOVIEPILOT_VAPID_SUBJECT"),
		WebPushUsernames: parseCSV(lookup("MOVIEPILOT_WEBPUSH_USERNAMES")),

		LoginMaxFailures:   defaultLoginMaxFailures,
		LoginIPMaxFailures: defaultLoginIPMaxFailures,
		LoginIPWindow:      defaultLoginIPWindow,
		LoginLockBase:      defaultLoginLockBase,
		LoginLockMax:       defaultLoginLockMax,
	}

	tzName := defaultString(lookup("MOVIEPILOT_TIMEZONE", "TZ"), defaultTimezone)
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return Config{}, fmt.Errorf("无效时区 %q: %w", tzName, err)
	}
	cfg.Location = loc

	if err := parseInt32Field(lookup("MOVIEPILOT_POSTGRES_MAX_CONNS"), &cfg.PostgresMaxConns, "MOVIEPILOT_POSTGRES_MAX_CONNS"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_REDIS_POOL_SIZE"), &cfg.RedisPoolSize, "MOVIEPILOT_REDIS_POOL_SIZE"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_MAX_CACHE_VALUE"), &cfg.MaxCacheValue, "MOVIEPILOT_MAX_CACHE_VALUE"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_ACCESS_TOKEN_LIFETIME"), &cfg.AccessTokenLifetime, "MOVIEPILOT_ACCESS_TOKEN_LIFETIME"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_REQUEST_TIMEOUT"), &cfg.RequestTimeout, "MOVIEPILOT_REQUEST_TIMEOUT"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_SHUTDOWN_TIMEOUT"), &cfg.ShutdownTimeout, "MOVIEPILOT_SHUTDOWN_TIMEOUT"); err != nil {
		return Config{}, err
	}
	if err := parseInt64Field(lookup("MOVIEPILOT_MAX_REQUEST_BODY"), &cfg.MaxRequestBody, "MOVIEPILOT_MAX_REQUEST_BODY"); err != nil {
		return Config{}, err
	}
	if err := parseInt64Field(lookup("MOVIEPILOT_MAX_RESPONSE_BODY"), &cfg.MaxResponseBody, "MOVIEPILOT_MAX_RESPONSE_BODY"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_SEARCH_WORKERS"), &cfg.SearchWorkers, "MOVIEPILOT_SEARCH_WORKERS"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_ORGANIZE_WORKERS"), &cfg.OrganizeWorkers, "MOVIEPILOT_ORGANIZE_WORKERS"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_SCRAPE_WORKERS"), &cfg.ScrapeWorkers, "MOVIEPILOT_SCRAPE_WORKERS"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_METADATA_WORKERS"), &cfg.MetadataWorkers, "MOVIEPILOT_METADATA_WORKERS"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_METADATA_TIMEOUT"), &cfg.MetadataTimeout, "MOVIEPILOT_METADATA_TIMEOUT"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_METADATA_CACHE_TTL"), &cfg.MetadataCacheTTL, "MOVIEPILOT_METADATA_CACHE_TTL"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_METADATA_MIN_INTERVAL"), &cfg.MetadataMinInterval, "MOVIEPILOT_METADATA_MIN_INTERVAL"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_METADATA_MAX_BACKOFF"), &cfg.MetadataMaxBackoff, "MOVIEPILOT_METADATA_MAX_BACKOFF"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_METADATA_MAX_RESULTS"), &cfg.MetadataMaxResults, "MOVIEPILOT_METADATA_MAX_RESULTS"); err != nil {
		return Config{}, err
	}
	if raw := lookup("SUBSCRIBE_RSS_INTERVAL", "MOVIEPILOT_SUBSCRIBE_RSS_INTERVAL"); raw != "" {
		interval, parseErr := parseRSSInterval(raw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("无效 SUBSCRIBE_RSS_INTERVAL %q: %w", raw, parseErr)
		}
		cfg.SubscribeRSSInterval = interval
	}
	if raw := lookup("COOKIECLOUD_INTERVAL"); raw != "" {
		interval, parseErr := parseMinutesOrDuration(raw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("无效 COOKIECLOUD_INTERVAL %q: %w", raw, parseErr)
		}
		cfg.CookieCloudInterval = interval
	}
	if err := parseIntField(lookup("MOVIEPILOT_LOGIN_MAX_FAILURES"), &cfg.LoginMaxFailures, "MOVIEPILOT_LOGIN_MAX_FAILURES"); err != nil {
		return Config{}, err
	}
	if err := parseIntField(lookup("MOVIEPILOT_LOGIN_IP_MAX_FAILURES"), &cfg.LoginIPMaxFailures, "MOVIEPILOT_LOGIN_IP_MAX_FAILURES"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_LOGIN_IP_WINDOW"), &cfg.LoginIPWindow, "MOVIEPILOT_LOGIN_IP_WINDOW"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_LOGIN_LOCK_BASE"), &cfg.LoginLockBase, "MOVIEPILOT_LOGIN_LOCK_BASE"); err != nil {
		return Config{}, err
	}
	if err := parseDurationField(lookup("MOVIEPILOT_LOGIN_LOCK_MAX"), &cfg.LoginLockMax, "MOVIEPILOT_LOGIN_LOCK_MAX"); err != nil {
		return Config{}, err
	}

	if isProd {
		if cfg.DatabaseURL == "" {
			return Config{}, errors.New("生产环境必须设置 MOVIEPILOT_DATABASE_URL")
		}
		if cfg.JWTSecret == "" {
			return Config{}, errors.New("生产环境必须设置 MOVIEPILOT_JWT_SECRET")
		}
		if cfg.ResourceSecret == "" {
			return Config{}, errors.New("生产环境必须设置 MOVIEPILOT_RESOURCE_SECRET")
		}
		if len(cfg.MasterKey) < 16 {
			return Config{}, errors.New("生产环境 MOVIEPILOT_MASTER_KEY 长度不能少于 16 字节")
		}
		if cfg.BootstrapPassword == "" {
			return Config{}, errors.New("生产环境必须设置 MOVIEPILOT_BOOTSTRAP_PASSWORD")
		}
	} else {
		if cfg.DatabaseURL == "" {
			cfg.DatabaseURL = defaultDatabaseURLDev
		}
		if cfg.JWTSecret == "" {
			secret, genErr := randomHexSecret(32)
			if genErr != nil {
				return Config{}, fmt.Errorf("生成临时 JWT 密钥: %w", genErr)
			}
			cfg.JWTSecret = secret
		}
		if cfg.ResourceSecret == "" {
			secret, genErr := randomHexSecret(32)
			if genErr != nil {
				return Config{}, fmt.Errorf("生成临时资源密钥: %w", genErr)
			}
			cfg.ResourceSecret = secret
		}
		if cfg.MasterKey == "" {
			cfg.MasterKey = defaultDevMasterKey
		}
		if cfg.BootstrapPassword == "" {
			cfg.BootstrapPassword = defaultBootstrapPassword
		}
	}

	return cfg, nil
}

func loadEnvFile(path string) (map[string]string, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return values, nil
}

func resolveTMDBBaseURL(explicit, domain string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	if domain != "" {
		if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
			return strings.TrimRight(domain, "/")
		}
		return "https://" + strings.TrimRight(domain, "/")
	}
	return defaultTMDBBaseURL
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseBoolDefault(raw string, fallback bool) bool {
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(strings.ToLower(raw))
	if err != nil {
		return fallback
	}
	return parsed
}

func parseCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseIntField(raw string, target *int, name string) error {
	if raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("无效 %s %q: %w", name, raw, err)
	}
	*target = value
	return nil
}

func parseInt32Field(raw string, target *int32, name string) error {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return fmt.Errorf("无效 %s %q: %w", name, raw, err)
	}
	*target = int32(value)
	return nil
}

func parseInt64Field(raw string, target *int64, name string) error {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("无效 %s %q: %w", name, raw, err)
	}
	*target = value
	return nil
}

func parseDurationField(raw string, target *time.Duration, name string) error {
	if raw == "" {
		return nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("无效 %s %q: %w", name, raw, err)
	}
	*target = value
	return nil
}

func parseMinutesOrDuration(raw string) (time.Duration, error) {
	if minutes, err := strconv.Atoi(raw); err == nil {
		if minutes <= 0 {
			return 0, errors.New("必须大于 0")
		}
		return time.Duration(minutes) * time.Minute, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("必须大于 0")
	}
	return d, nil
}

func parseRSSInterval(raw string) (time.Duration, error) {
	return parseMinutesOrDuration(raw)
}

func randomHexSecret(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
