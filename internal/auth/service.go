package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// User 是鉴权边界需要的最小用户模型。
type User struct {
	ID          int64          `json:"id"`
	Username    string         `json:"name"`
	Password    string         `json:"-"`
	SuperUser   bool           `json:"is_superuser"`
	Avatar      *string        `json:"avatar"`
	Permissions map[string]any `json:"permissions"`
}

// UserStore 定义鉴权服务所需的持久化操作。
type UserStore interface {
	UserByName(ctx context.Context, username string) (User, error)
	UserByID(ctx context.Context, id int64) (User, error)
}

// Claims 是兼容 V2 的 JWT 载荷。
type Claims struct {
	Username  string `json:"username"`
	SuperUser bool   `json:"super_user"`
	Level     int    `json:"level"`
	Purpose   string `json:"purpose"`
	jwt.RegisteredClaims
}

// Service 负责密码验证和访问令牌签发。
type Service struct {
	store         UserStore
	jwtSecret     []byte
	resourceKey   []byte
	tokenLifetime time.Duration
}

// NewService 创建鉴权服务。
func NewService(store UserStore, jwtSecret, resourceSecret string, tokenLifetime time.Duration) *Service {
	return &Service{
		store: store, jwtSecret: []byte(jwtSecret), resourceKey: []byte(resourceSecret), tokenLifetime: tokenLifetime,
	}
}

// HashPassword 使用 bcrypt 生成不可逆密码摘要。
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("密码至少需要 8 个字符")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", fmt.Errorf("生成密码摘要: %w", err)
	}
	return string(hash), nil
}

// Authenticate 校验用户名和密码；失败时不暴露账户是否存在。
func (s *Service) Authenticate(ctx context.Context, username, password string) (User, error) {
	user, err := s.store.UserByName(ctx, strings.TrimSpace(username))
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) != nil {
		return User{}, errors.New("用户名或密码错误")
	}
	return user, nil
}

// UserByName 通过用户名查询用户信息（用于 Passkey 等无密码登录流程）。
func (s *Service) UserByName(ctx context.Context, username string) (User, error) {
	if s == nil || s.store == nil {
		return User{}, errors.New("用户存储不可用")
	}
	return s.store.UserByName(ctx, strings.TrimSpace(username))
}

// UserByID 通过用户 ID 查询用户信息。
func (s *Service) UserByID(ctx context.Context, id int64) (User, error) {
	if s == nil || s.store == nil {
		return User{}, errors.New("用户存储不可用")
	}
	return s.store.UserByID(ctx, id)
}

// AccessToken 为已认证用户签发 HS256 Bearer Token。
func (s *Service) AccessToken(user User) (string, error) {
	return s.sign(user, "authentication", s.jwtSecret, s.tokenLifetime)
}

// ResourceToken 为图片和文件资源签发独立用途令牌。
func (s *Service) ResourceToken(user User, lifetime time.Duration) (string, error) {
	return s.sign(user, "resource", s.resourceKey, lifetime)
}

// ResourceTokenFromClaims 用已校验的身份重新签发资源令牌。
//
// `<img src>` / `<video src>` 只带 Cookie 不带 Authorization，V2 在每次鉴权请求里
// 调 `set_or_refresh_resource_token_cookie` 顺手续期，这里对应同一步。
func (s *Service) ResourceTokenFromClaims(claims Claims, lifetime time.Duration) (string, error) {
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		return "", errors.New("资源令牌身份无效")
	}
	return s.sign(User{ID: id, Username: claims.Username, SuperUser: claims.SuperUser},
		"resource", s.resourceKey, lifetime)
}

// VerifyAccessToken 校验访问令牌并返回载荷。
func (s *Service) VerifyAccessToken(raw string) (Claims, error) {
	return s.verify(raw, "authentication", s.jwtSecret)
}

// VerifyResourceToken 校验资源令牌并返回载荷。
func (s *Service) VerifyResourceToken(raw string) (Claims, error) {
	return s.verify(raw, "resource", s.resourceKey)
}

func (s *Service) sign(user User, purpose string, key []byte, lifetime time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := Claims{
		Username: user.Username, SuperUser: user.SuperUser, Level: 1, Purpose: purpose,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: strconv.FormatInt(user.ID, 10), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
}

func (s *Service) verify(raw, purpose string, key []byte) (Claims, error) {
	claims := Claims{}
	token, err := jwt.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("令牌签名算法无效")
		}
		return key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || claims.Purpose != purpose {
		return Claims{}, errors.New("token校验不通过")
	}
	return claims, nil
}
