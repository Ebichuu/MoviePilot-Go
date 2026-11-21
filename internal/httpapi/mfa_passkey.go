package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const (
	passkeyChallengeTTL     = 5 * time.Minute
	passkeyMaxChallenges    = 1024
	passkeySettingKeyPrefix = "UserPasskeys:"
	passkeyIndexSettingKey  = "PasskeyCredentialIndex"
	defaultPasskeyDisplayRP = "MoviePilot"
	defaultPasskeyKeyName   = "通行密钥"
)

// storedPasskey 是单个用户 Passkey 凭证的加密持久化结构。
type storedPasskey struct {
	ID           int64               `json:"id"`
	CredentialID string              `json:"credential_id"`
	Name         string              `json:"name"`
	CreatedAt    time.Time           `json:"created_at"`
	LastUsedAt   *time.Time          `json:"last_used_at,omitempty"`
	AAGUID       string              `json:"aaguid,omitempty"`
	Transports   []string            `json:"transports,omitempty"`
	Credential   webauthn.Credential `json:"credential"`
}

type storedUserPasskeys struct {
	NextID int64           `json:"next_id"`
	Keys   []storedPasskey `json:"keys"`
}

type passkeyChallengeState struct {
	Purpose   string
	UserID    int64
	Username  string
	RPID      string
	RPOrigins []string
	Session   webauthn.SessionData
	ExpiresAt time.Time
}

// PasskeyChallengeStore 管理短期、一次性消费的 WebAuthn 挑战事务令牌。
type PasskeyChallengeStore struct {
	mu      sync.Mutex
	entries map[string]passkeyChallengeState
	now     func() time.Time
}

// NewPasskeyChallengeStore 创建带默认时钟的 Passkey 挑战存储。
func NewPasskeyChallengeStore() *PasskeyChallengeStore {
	return &PasskeyChallengeStore{
		entries: make(map[string]passkeyChallengeState),
		now:     time.Now,
	}
}

func (s *PasskeyChallengeStore) issue(state passkeyChallengeState) (string, error) {
	if s == nil {
		return "", errors.New("passkey challenge store unavailable")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	state.ExpiresAt = now.Add(passkeyChallengeTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]passkeyChallengeState)
	}
	// 清理过期条目，防止异常流量导致内存增长。
	for k, v := range s.entries {
		if now.After(v.ExpiresAt) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= passkeyMaxChallenges {
		// 淘汰最早过期的一条，保持总容量有界。
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.entries {
			if oldestKey == "" || v.ExpiresAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.ExpiresAt
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	s.entries[token] = state
	return token, nil
}

func (s *PasskeyChallengeStore) consume(token, purpose string) (passkeyChallengeState, bool) {
	if s == nil {
		return passkeyChallengeState{}, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return passkeyChallengeState{}, false
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.entries[token]
	if !ok {
		return passkeyChallengeState{}, false
	}
	delete(s.entries, token)
	if state.Purpose != purpose || now.After(state.ExpiresAt) {
		return passkeyChallengeState{}, false
	}
	return state, true
}

// webAuthnUser 适配 auth.User 与 storedPasskey 到 go-webauthn 的 webauthn.User 契约。
type webAuthnUser struct {
	id          int64
	username    string
	credentials []webauthn.Credential
}

func (u webAuthnUser) WebAuthnID() []byte                         { return []byte(strconv.FormatInt(u.id, 10)) }
func (u webAuthnUser) WebAuthnName() string                       { return u.username }
func (u webAuthnUser) WebAuthnDisplayName() string                { return u.username }
func (u webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func newWebAuthnUser(user auth.User, stored []storedPasskey) webAuthnUser {
	creds := make([]webauthn.Credential, 0, len(stored))
	for _, item := range stored {
		creds = append(creds, item.Credential)
	}
	return webAuthnUser{
		id:          user.ID,
		username:    user.Username,
		credentials: creds,
	}
}

func (d Dependencies) passkeySettingStore() UserConfigStore {
	if d.Store != nil {
		return d.Store
	}
	return d.UserConfigs
}

func (d Dependencies) passkeyChallenges() *PasskeyChallengeStore {
	if d.PasskeyChallenges != nil {
		return d.PasskeyChallenges
	}
	return defaultPasskeyChallenges
}

var defaultPasskeyChallenges = NewPasskeyChallengeStore()

func (d Dependencies) loadUserPasskeys(ctx context.Context, username string) (storedUserPasskeys, error) {
	data := storedUserPasskeys{}
	store := d.passkeySettingStore()
	if store == nil || strings.TrimSpace(username) == "" {
		return data, nil
	}
	found, err := store.GetSetting(ctx, passkeySettingKeyPrefix+strings.TrimSpace(username), &data)
	if err != nil || !found {
		return storedUserPasskeys{}, err
	}
	return data, nil
}

func (d Dependencies) saveUserPasskeys(ctx context.Context, username string, data storedUserPasskeys) error {
	store := d.passkeySettingStore()
	if store == nil {
		return errors.New("系统设置存储不可用")
	}
	return store.SetSetting(ctx, passkeySettingKeyPrefix+strings.TrimSpace(username), data, true)
}

func (d Dependencies) loadPasskeyIndex(ctx context.Context) (map[string]string, error) {
	index := make(map[string]string)
	store := d.passkeySettingStore()
	if store == nil {
		return index, nil
	}
	found, err := store.GetSetting(ctx, passkeyIndexSettingKey, &index)
	if err != nil || !found || index == nil {
		return make(map[string]string), err
	}
	return index, nil
}

func (d Dependencies) savePasskeyIndex(ctx context.Context, index map[string]string) error {
	store := d.passkeySettingStore()
	if store == nil {
		return errors.New("系统设置存储不可用")
	}
	return store.SetSetting(ctx, passkeyIndexSettingKey, index, true)
}

func (d Dependencies) resolvePasskeyRP(r *http.Request) (string, []string) {
	if raw := strings.TrimSpace(d.Config.AppDomain); raw != "" {
		if rpID, origin, ok := parseRPFromOriginOrURL(raw); ok {
			return rpID, []string{origin}
		}
	}
	if r != nil {
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			if rpID, normOrigin, ok := parseRPFromOriginOrURL(origin); ok {
				return rpID, []string{normOrigin}
			}
		}
		host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
		if host == "" {
			host = strings.TrimSpace(r.Host)
		}
		if host != "" {
			scheme := "http"
			if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
				scheme = "https"
			}
			if rpID, normOrigin, ok := parseRPFromOriginOrURL(scheme + "://" + host); ok {
				return rpID, []string{normOrigin}
			}
		}
	}
	return "localhost", []string{"http://localhost"}
}

func parseRPFromOriginOrURL(raw string) (string, string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", false
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", "", false
	}
	// 去除 IPv6 方括号之外的多余空白；WebAuthn RP ID 必须是有效域名或 localhost。
	if ip := net.ParseIP(hostname); ip != nil && hostname == "127.0.0.1" {
		// 本地 IP 场景保持 hostname 原值，避免与浏览器 origin 不一致。
	}
	origin := scheme + "://" + parsed.Host
	return hostname, origin, true
}

func newWebAuthnInstance(rpID string, rpOrigins []string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPDisplayName: defaultPasskeyDisplayRP,
		RPID:          rpID,
		RPOrigins:     rpOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	})
}

func (d Dependencies) currentAuthUser(r *http.Request) (auth.User, error) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		return auth.User{}, errors.New("未登录")
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		return auth.User{}, errors.New("无效的用户身份")
	}
	if d.Store != nil {
		if user, err := d.Store.UserByID(r.Context(), id); err == nil {
			return user, nil
		}
	}
	if d.Users != nil {
		if user, err := d.Users.UserByID(r.Context(), id); err == nil {
			return user, nil
		}
	}
	if d.Auth != nil {
		if user, err := d.Auth.UserByID(r.Context(), id); err == nil {
			return user, nil
		}
	}
	if strings.TrimSpace(claims.Username) != "" {
		return auth.User{ID: id, Username: claims.Username, SuperUser: claims.SuperUser}, nil
	}
	return auth.User{}, errors.New("用户不存在")
}

func (d Dependencies) lookupUserByName(ctx context.Context, username string) (auth.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return auth.User{}, errors.New("用户名为空")
	}
	if d.Store != nil {
		return d.Store.UserByName(ctx, username)
	}
	if d.Auth != nil {
		return d.Auth.UserByName(ctx, username)
	}
	return auth.User{}, errors.New("用户存储不可用")
}

// passkeyRegisterStart 对应 V2 `POST /api/v1/mfa/passkey/register/start`：生成 WebAuthn 注册选项。
func (d Dependencies) passkeyRegisterStart(w http.ResponseWriter, r *http.Request) {
	user, err := d.currentAuthUser(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "无法识别当前用户"})
		return
	}
	existing, err := d.loadUserPasskeys(r.Context(), user.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取通行密钥失败"})
		return
	}
	rpID, rpOrigins := d.resolvePasskeyRP(r)
	wa, err := newWebAuthnInstance(rpID, rpOrigins)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "生成注册选项失败"})
		return
	}
	waUser := newWebAuthnUser(user, existing.Keys)
	exclusions := make([]protocol.CredentialDescriptor, 0, len(existing.Keys))
	for _, item := range existing.Keys {
		exclusions = append(exclusions, item.Credential.Descriptor())
	}
	creation, session, err := wa.BeginRegistration(
		waUser,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
	)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "生成注册选项失败"})
		return
	}
	optionsBytes, err := json.Marshal(creation.Response)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "生成注册选项失败"})
		return
	}
	txToken, err := d.passkeyChallenges().issue(passkeyChallengeState{
		Purpose:   "registration",
		UserID:    user.ID,
		Username:  user.Username,
		RPID:      rpID,
		RPOrigins: rpOrigins,
		Session:   *session,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "生成注册会话失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: map[string]any{
			"options":           string(optionsBytes),
			"transaction_token": txToken,
		},
	})
}

// passkeyRegisterFinish 对应 V2 `POST /api/v1/mfa/passkey/register/finish`：校验注册响应并加密保存凭证。
func (d Dependencies) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	user, err := d.currentAuthUser(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "无法识别当前用户"})
		return
	}
	var req struct {
		Credential       json.RawMessage `json:"credential"`
		TransactionToken string          `json:"transaction_token"`
		Name             string          `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Credential) == 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "注册参数无效"})
		return
	}
	state, ok := d.passkeyChallenges().consume(req.TransactionToken, "registration")
	if !ok || state.UserID != user.ID {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "注册请求已失效，请重新发起注册"})
		return
	}
	wa, err := newWebAuthnInstance(state.RPID, state.RPOrigins)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "通行密钥注册失败，请稍后重试"})
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "通行密钥注册验证失败，请重新发起注册后重试"})
		return
	}
	existing, err := d.loadUserPasskeys(r.Context(), user.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取通行密钥失败"})
		return
	}
	waUser := newWebAuthnUser(user, existing.Keys)
	cred, err := wa.CreateCredential(waUser, state.Session, parsed)
	if err != nil {
		msg := "通行密钥注册验证失败，请重新发起注册后重试"
		if strings.Contains(strings.ToLower(err.Error()), "origin") {
			msg = "访问域名与系统配置不一致，请使用配置的域名重试"
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: msg})
		return
	}

	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultPasskeyKeyName
	}
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		if s := strings.TrimSpace(string(t)); s != "" {
			transports = append(transports, s)
		}
	}
	aaguid := ""
	if len(cred.Authenticator.AAGUID) > 0 {
		aaguid = formatAAGUID(cred.Authenticator.AAGUID)
	}

	existing.NextID++
	if existing.NextID <= 0 {
		existing.NextID = 1
	}
	existing.Keys = append(existing.Keys, storedPasskey{
		ID:           existing.NextID,
		CredentialID: credID,
		Name:         name,
		CreatedAt:    time.Now().UTC(),
		AAGUID:       aaguid,
		Transports:   transports,
		Credential:   *cred,
	})
	if err := d.saveUserPasskeys(r.Context(), user.Username, existing); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存通行密钥失败"})
		return
	}
	index, err := d.loadPasskeyIndex(r.Context())
	if err == nil {
		index[credID] = user.Username
		_ = d.savePasskeyIndex(r.Context(), index)
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "通行密钥注册成功"})
}

// passkeyAuthenticateStart 对应 V2 `POST /api/v1/mfa/passkey/authenticate/start`：生成登录认证选项。
func (d Dependencies) passkeyAuthenticateStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	username := strings.TrimSpace(req.Username)

	rpID, rpOrigins := d.resolvePasskeyRP(r)
	wa, err := newWebAuthnInstance(rpID, rpOrigins)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
		return
	}

	var (
		assertion *protocol.CredentialAssertion
		session   *webauthn.SessionData
		userID    int64
	)
	if username != "" {
		user, err := d.lookupUserByName(r.Context(), username)
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
			return
		}
		stored, err := d.loadUserPasskeys(r.Context(), user.Username)
		if err != nil || len(stored.Keys) == 0 {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
			return
		}
		waUser := newWebAuthnUser(user, stored.Keys)
		assertion, session, err = wa.BeginLogin(waUser)
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
			return
		}
		userID = user.ID
	} else {
		assertion, session, err = wa.BeginDiscoverableLogin()
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
			return
		}
	}

	optionsBytes, err := json.Marshal(assertion.Response)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
		return
	}
	txToken, err := d.passkeyChallenges().issue(passkeyChallengeState{
		Purpose:   "authentication",
		UserID:    userID,
		Username:  username,
		RPID:      rpID,
		RPOrigins: rpOrigins,
		Session:   *session,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "认证失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Data: map[string]any{
			"options":           string(optionsBytes),
			"transaction_token": txToken,
		},
	})
}

// passkeyAuthenticateFinish 对应 V2 `POST /api/v1/mfa/passkey/authenticate/finish`：校验签名并签发登录令牌。
func (d Dependencies) passkeyAuthenticateFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Credential       json.RawMessage `json:"credential"`
		TransactionToken string          `json:"transaction_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Credential) == 0 {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	state, ok := d.passkeyChallenges().consume(req.TransactionToken, "authentication")
	if !ok {
		writeError(w, http.StatusUnauthorized, "认证请求已失效")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if err != nil || len(parsed.RawID) == 0 {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	credID := base64.RawURLEncoding.EncodeToString(parsed.RawID)
	username := state.Username
	if username == "" {
		index, err := d.loadPasskeyIndex(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, "认证失败")
			return
		}
		username = strings.TrimSpace(index[credID])
	}
	if username == "" {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}

	user, err := d.lookupUserByName(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	if state.UserID != 0 && state.UserID != user.ID {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	stored, err := d.loadUserPasskeys(r.Context(), user.Username)
	if err != nil || len(stored.Keys) == 0 {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}

	wa, err := newWebAuthnInstance(state.RPID, state.RPOrigins)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}
	waUser := newWebAuthnUser(user, stored.Keys)
	var validated *webauthn.Credential
	if state.UserID != 0 {
		validated, err = wa.ValidateLogin(waUser, state.Session, parsed)
	} else {
		validated, err = wa.ValidateDiscoverableLogin(func(_, _ []byte) (webauthn.User, error) {
			return waUser, nil
		}, state.Session, parsed)
	}
	if err != nil || validated == nil {
		writeError(w, http.StatusUnauthorized, "认证失败")
		return
	}

	now := time.Now().UTC()
	for i := range stored.Keys {
		if stored.Keys[i].CredentialID == credID {
			stored.Keys[i].Credential = *validated
			stored.Keys[i].LastUsedAt = &now
			break
		}
	}
	_ = d.saveUserPasskeys(r.Context(), user.Username, stored)

	d.issueLoginTokenResponse(w, r, user)
}

// passkeyList 对应 V2 `GET /api/v1/mfa/passkey/list`：返回当前用户已绑定的通行密钥列表。
func (d Dependencies) passkeyList(w http.ResponseWriter, r *http.Request) {
	user, err := d.currentAuthUser(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "无法识别当前用户"})
		return
	}
	stored, err := d.loadUserPasskeys(r.Context(), user.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "获取列表失败"})
		return
	}
	items := make([]map[string]any, 0, len(stored.Keys))
	for _, key := range stored.Keys {
		var lastUsed any
		if key.LastUsedAt != nil {
			lastUsed = key.LastUsedAt.Format(time.RFC3339)
		}
		var transports any
		if len(key.Transports) > 0 {
			transports = strings.Join(key.Transports, ",")
		}
		var aaguid any
		if key.AAGUID != "" {
			aaguid = key.AAGUID
		}
		items = append(items, map[string]any{
			"id":           key.ID,
			"name":         key.Name,
			"created_at":   key.CreatedAt.Format(time.RFC3339),
			"last_used_at": lastUsed,
			"aaguid":       aaguid,
			"transports":   transports,
		})
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: items})
}

// passkeyDelete 对应 V2 `POST /api/v1/mfa/passkey/delete`：校验登录密码后删除指定通行密钥。
func (d Dependencies) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	user, err := d.currentAuthUser(r)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "无法识别当前用户"})
		return
	}
	var req struct {
		PasskeyID json.RawMessage `json:"passkey_id"`
		Password  string          `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "删除参数无效"})
		return
	}
	if d.Auth != nil {
		if _, err := d.Auth.Authenticate(r.Context(), user.Username, req.Password); err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "密码错误"})
			return
		}
	}
	targetID, targetCredID := parsePasskeyDeleteTarget(req.PasskeyID)
	stored, err := d.loadUserPasskeys(r.Context(), user.Username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "删除失败"})
		return
	}
	removedCredID := ""
	kept := make([]storedPasskey, 0, len(stored.Keys))
	for _, key := range stored.Keys {
		match := (targetID > 0 && key.ID == targetID) || (targetCredID != "" && key.CredentialID == targetCredID)
		if match && removedCredID == "" {
			removedCredID = key.CredentialID
			continue
		}
		kept = append(kept, key)
	}
	if removedCredID == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "通行密钥不存在或无权删除"})
		return
	}
	stored.Keys = kept
	if err := d.saveUserPasskeys(r.Context(), user.Username, stored); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "删除失败"})
		return
	}
	if index, err := d.loadPasskeyIndex(r.Context()); err == nil {
		delete(index, removedCredID)
		_ = d.savePasskeyIndex(r.Context(), index)
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Message: "通行密钥已删除"})
}

func (d Dependencies) issueLoginTokenResponse(w http.ResponseWriter, r *http.Request, user auth.User) {
	if d.Auth == nil {
		writeError(w, http.StatusInternalServerError, "鉴权服务不可用")
		return
	}
	token, err := d.Auth.AccessToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成访问令牌失败")
		return
	}
	resourceToken, err := d.Auth.ResourceToken(user, resourceTokenLifetime)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成资源令牌失败")
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: resourceCookieName, Value: resourceToken, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	perms := user.Permissions
	if perms == nil {
		perms = map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "token_type": "bearer", "super_user": user.SuperUser,
		"user_id": user.ID, "user_name": user.Username, "avatar": user.Avatar,
		"level": 1, "permissions": perms, "wizard": false,
	})
}

func parsePasskeyDeleteTarget(raw json.RawMessage) (int64, string) {
	var num int64
	if err := json.Unmarshal(raw, &num); err == nil && num > 0 {
		return num, ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if parsed, err := strconv.ParseInt(s, 10, 64); err == nil && parsed > 0 {
			return parsed, s
		}
		return 0, s
	}
	return 0, ""
}

func formatAAGUID(raw []byte) string {
	if len(raw) != 16 {
		return hex.EncodeToString(raw)
	}
	h := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
