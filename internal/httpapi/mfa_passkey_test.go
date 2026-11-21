package httpapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

type memoryPasskeyUserStore struct {
	users map[string]auth.User
}

func (m *memoryPasskeyUserStore) UserByName(_ context.Context, username string) (auth.User, error) {
	u, ok := m.users[username]
	if !ok {
		return auth.User{}, errors.New("not found")
	}
	return u, nil
}

func (m *memoryPasskeyUserStore) UserByID(_ context.Context, id int64) (auth.User, error) {
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return auth.User{}, errors.New("not found")
}

type memorySettingsKV struct {
	data map[string][]byte
}

func (m *memorySettingsKV) GetSetting(_ context.Context, key string, dest any) (bool, error) {
	raw, ok := m.data[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dest)
}

func (m *memorySettingsKV) SetSetting(_ context.Context, key string, value any, _ bool) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	m.data[key] = raw
	return nil
}

// syntheticAuthenticator 用纯 Go ECDSA P-256 模拟真实浏览器的 WebAuthn 认证器。
type syntheticAuthenticator struct {
	key    *ecdsa.PrivateKey
	credID []byte
	rpID   string
	origin string
	count  uint32
}

func newSyntheticAuthenticator(t *testing.T, rpID, origin string) *syntheticAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	return &syntheticAuthenticator{key: priv, credID: credID, rpID: rpID, origin: origin}
}

func (a *syntheticAuthenticator) buildRegistrationCredential(t *testing.T, challenge string) map[string]any {
	t.Helper()
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.create",
		"challenge": challenge,
		"origin":    a.origin,
	})
	if err != nil {
		t.Fatalf("marshal clientData: %v", err)
	}

	// COSE EC2 P-256 ES256 public key map
	coseKey := map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: pad32(a.key.PublicKey.X.Bytes()),
		-3: pad32(a.key.PublicKey.Y.Bytes()),
	}
	coseBytes, err := cbor.Marshal(coseKey)
	if err != nil {
		t.Fatalf("marshal cose key: %v", err)
	}

	rpHash := sha256.Sum256([]byte(a.rpID))
	var authData bytes.Buffer
	authData.Write(rpHash[:])
	// Flags: UP (0x01) | UV (0x04) | AT (0x40) = 0x45
	authData.WriteByte(0x45)
	_ = binary.Write(&authData, binary.BigEndian, a.count)
	// AAGUID (16 zero bytes)
	authData.Write(make([]byte, 16))
	_ = binary.Write(&authData, binary.BigEndian, uint16(len(a.credID)))
	authData.Write(a.credID)
	authData.Write(coseBytes)

	attObj, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData.Bytes(),
	})
	if err != nil {
		t.Fatalf("marshal attestationObject: %v", err)
	}

	credIDB64 := base64.RawURLEncoding.EncodeToString(a.credID)
	return map[string]any{
		"id":    credIDB64,
		"rawId": credIDB64,
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
			"transports":        []string{"internal"},
		},
	}
}

func (a *syntheticAuthenticator) buildAssertionCredential(t *testing.T, challenge string, userHandle []byte) map[string]any {
	t.Helper()
	a.count++
	clientData, err := json.Marshal(map[string]any{
		"type":      "webauthn.get",
		"challenge": challenge,
		"origin":    a.origin,
	})
	if err != nil {
		t.Fatalf("marshal clientData: %v", err)
	}

	rpHash := sha256.Sum256([]byte(a.rpID))
	var authData bytes.Buffer
	authData.Write(rpHash[:])
	// Flags: UP (0x01) | UV (0x04) = 0x05
	authData.WriteByte(0x05)
	_ = binary.Write(&authData, binary.BigEndian, a.count)

	clientDataHash := sha256.Sum256(clientData)
	signedPayload := append(append([]byte(nil), authData.Bytes()...), clientDataHash[:]...)
	digest := sha256.Sum256(signedPayload)
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}

	credIDB64 := base64.RawURLEncoding.EncodeToString(a.credID)
	return map[string]any{
		"id":    credIDB64,
		"rawId": credIDB64,
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData.Bytes()),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
			"userHandle":        base64.RawURLEncoding.EncodeToString(userHandle),
		},
	}
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func TestPasskeyFullLifecycleRegisterAuthenticateListAndDelete(t *testing.T) {
	hash, err := auth.HashPassword("StrongPass123!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user := auth.User{
		ID:          7,
		Username:    "alice",
		Password:    hash,
		SuperUser:   true,
		Permissions: map[string]any{"manage": true},
	}
	userStore := &memoryPasskeyUserStore{users: map[string]auth.User{"alice": user}}
	authSvc := auth.NewService(userStore, "test-jwt-secret-32-bytes-long!!", "test-res-secret-32-bytes-long!!", time.Hour)
	kv := &memorySettingsKV{}
	router := NewRouter(Dependencies{
		Config:            config.Config{AppDomain: "https://mp.example.com", MaxRequestBody: 1 << 20},
		Auth:              authSvc,
		Users:             userStore,
		UserConfigs:       kv,
		PasskeyChallenges: NewPasskeyChallengeStore(),
	})

	accessToken, err := authSvc.AccessToken(user)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	authenticator := newSyntheticAuthenticator(t, "mp.example.com", "https://mp.example.com")

	// 1. Register start
	regStartRec := httptest.NewRecorder()
	regStartReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/register/start", strings.NewReader(`{}`))
	regStartReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(regStartRec, regStartReq)
	if regStartRec.Code != http.StatusOK {
		t.Fatalf("register/start status = %d, body=%s", regStartRec.Code, regStartRec.Body.String())
	}
	var regStartBody struct {
		Success bool `json:"success"`
		Data    struct {
			Options          string `json:"options"`
			TransactionToken string `json:"transaction_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(regStartRec.Body.Bytes(), &regStartBody); err != nil {
		t.Fatalf("decode register/start: %v", err)
	}
	if !regStartBody.Success || regStartBody.Data.TransactionToken == "" || regStartBody.Data.Options == "" {
		t.Fatalf("unexpected register/start body: %+v", regStartBody)
	}
	var creationOpts struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(regStartBody.Data.Options), &creationOpts); err != nil || creationOpts.Challenge == "" {
		t.Fatalf("invalid creation options JSON: %q err=%v", regStartBody.Data.Options, err)
	}

	// 2. Register finish with real ECDSA WebAuthn attestation
	regPayload, _ := json.Marshal(map[string]any{
		"credential":        authenticator.buildRegistrationCredential(t, creationOpts.Challenge),
		"transaction_token": regStartBody.Data.TransactionToken,
		"name":              "MacBook TouchID",
	})
	regFinishRec := httptest.NewRecorder()
	regFinishReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/register/finish", bytes.NewReader(regPayload))
	regFinishReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(regFinishRec, regFinishReq)
	var regFinishBody Response
	if err := json.Unmarshal(regFinishRec.Body.Bytes(), &regFinishBody); err != nil {
		t.Fatalf("decode register/finish: %v", err)
	}
	if !regFinishBody.Success {
		t.Fatalf("register/finish failed: %+v", regFinishBody)
	}

	// 3. List passkeys & check MFA status
	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/passkey/list", nil)
	listReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(listRec, listReq)
	var listBody struct {
		Success bool `json:"success"`
		Data    []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !listBody.Success || len(listBody.Data) != 1 || listBody.Data[0].Name != "MacBook TouchID" || listBody.Data[0].ID != 1 {
		t.Fatalf("unexpected passkey list: %+v", listBody)
	}

	statusRec := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/status/alice", nil)
	statusReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(statusRec, statusReq)
	if !strings.Contains(statusRec.Body.String(), `"count":1`) || !strings.Contains(statusRec.Body.String(), `"enabled":true`) {
		t.Fatalf("expected MFA status to report 1 passkey, got %s", statusRec.Body.String())
	}

	// 4. Authenticate (both username-scoped and discoverable/usernameless) without JWT
	for _, startPayload := range []string{`{"username":"alice"}`, `{}`} {
		authStartRec := httptest.NewRecorder()
		authStartReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/authenticate/start", strings.NewReader(startPayload))
		router.ServeHTTP(authStartRec, authStartReq)
		var authStartBody struct {
			Success bool `json:"success"`
			Data    struct {
				Options          string `json:"options"`
				TransactionToken string `json:"transaction_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(authStartRec.Body.Bytes(), &authStartBody); err != nil || !authStartBody.Success {
			t.Fatalf("authenticate/start (%s) failed: %s", startPayload, authStartRec.Body.String())
		}
		var reqOpts struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal([]byte(authStartBody.Data.Options), &reqOpts); err != nil || reqOpts.Challenge == "" {
			t.Fatalf("invalid assertion options: %q", authStartBody.Data.Options)
		}

		finishPayload, _ := json.Marshal(map[string]any{
			"credential":        authenticator.buildAssertionCredential(t, reqOpts.Challenge, []byte("7")),
			"transaction_token": authStartBody.Data.TransactionToken,
		})
		authFinishRec := httptest.NewRecorder()
		authFinishReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/authenticate/finish", bytes.NewReader(finishPayload))
		router.ServeHTTP(authFinishRec, authFinishReq)
		if authFinishRec.Code != http.StatusOK {
			t.Fatalf("authenticate/finish (%s) status=%d body=%s", startPayload, authFinishRec.Code, authFinishRec.Body.String())
		}
		var tokenResp map[string]any
		if err := json.Unmarshal(authFinishRec.Body.Bytes(), &tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tok, _ := tokenResp["access_token"].(string); tok == "" || tokenResp["user_name"] != "alice" {
			t.Fatalf("unexpected token response: %+v", tokenResp)
		}
		if !strings.Contains(authFinishRec.Header().Get("Set-Cookie"), resourceCookieName+"=") {
			t.Fatalf("expected resource cookie on passkey login, headers=%v", authFinishRec.Header())
		}

		// Replay of consumed transaction_token must fail with 401
		replayRec := httptest.NewRecorder()
		router.ServeHTTP(replayRec, httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/authenticate/finish", bytes.NewReader(finishPayload)))
		if replayRec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 on replay, got %d", replayRec.Code)
		}
	}

	// 5. Delete passkey: wrong password rejected, right password succeeds
	badDelRec := httptest.NewRecorder()
	badDelReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/delete", strings.NewReader(`{"passkey_id":1,"password":"wrong"}`))
	badDelReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(badDelRec, badDelReq)
	var badDelBody Response
	_ = json.Unmarshal(badDelRec.Body.Bytes(), &badDelBody)
	if badDelBody.Success || badDelBody.Message != "密码错误" {
		t.Fatalf("expected password error, got %+v", badDelBody)
	}

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/delete", strings.NewReader(`{"passkey_id":1,"password":"StrongPass123!"}`))
	delReq.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(delRec, delReq)
	var delBody Response
	_ = json.Unmarshal(delRec.Body.Bytes(), &delBody)
	if !delBody.Success {
		t.Fatalf("expected delete success, got %+v", delBody)
	}

	// Confirm MFA status is now 0 passkeys
	statusRec2 := httptest.NewRecorder()
	statusReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/status/alice", nil)
	statusReq2.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(statusRec2, statusReq2)
	if !strings.Contains(statusRec2.Body.String(), `"count":0`) {
		t.Fatalf("expected MFA status count=0 after delete, got %s", statusRec2.Body.String())
	}
}
