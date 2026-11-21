package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// mfaRecord 是用户 MFA 的持久化状态（存放在 settings 表，按用户名区分）。
type mfaRecord struct {
	Secret  string `json:"secret"`
	Enabled bool   `json:"enabled"`
}

func mfaSettingKey(username string) string { return "UserMFA:" + username }

// mfaOTPGenerate 对应 V2 `POST /mfa/otp/generate`：生成 TOTP 密钥并返回 otpauth URI。
func (d Dependencies) mfaOTPGenerate(w http.ResponseWriter, r *http.Request) {
	in := readMFAInput(r)
	username, ok := in.Username, in.Username != ""
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户名不能为空"})
		return
	}
	secret, err := mfaNewSecret()
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "生成密钥失败"})
		return
	}
	record := mfaRecord{Secret: secret}
	if err := d.setMFARecord(r, username, record); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存密钥失败"})
		return
	}
	uri := fmt.Sprintf("otpauth://totp/MoviePilot:%s?secret=%s&issuer=MoviePilot", username, secret)
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{
		"secret": secret, "otpauth_url": uri, "enabled": false,
	}})
}

// mfaOTPVerify 对应 V2 `POST /mfa/otp/verify`：校验动态码并启用。
func (d Dependencies) mfaOTPVerify(w http.ResponseWriter, r *http.Request) {
	in := readMFAInput(r)
	username, ok := in.Username, in.Username != ""
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户名不能为空"})
		return
	}
	record, found, err := d.getMFARecord(r, username)
	if err != nil || !found || record.Secret == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请先生成 MFA 密钥"})
		return
	}
	if !mfaValidateCode(record.Secret, in.Code, time.Now()) {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "验证码错误"})
		return
	}
	record.Enabled = true
	if err := d.setMFARecord(r, username, record); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存 MFA 状态失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"enabled": true}})
}

// mfaOTPDisable 对应 V2 `POST /mfa/otp/disable`：校验动态码后关闭 MFA。
func (d Dependencies) mfaOTPDisable(w http.ResponseWriter, r *http.Request) {
	in := readMFAInput(r)
	username, ok := in.Username, in.Username != ""
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户名不能为空"})
		return
	}
	record, found, err := d.getMFARecord(r, username)
	if err != nil || !found || record.Secret == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未启用 MFA"})
		return
	}
	if !mfaValidateCode(record.Secret, in.Code, time.Now()) {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "验证码错误"})
		return
	}
	if err := d.setMFARecord(r, username, mfaRecord{}); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存 MFA 状态失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"enabled": false}})
}

// mfaStatus 对应 V2 `GET /mfa/status/{username}`。
func (d Dependencies) mfaStatus(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(chi.URLParam(r, "username"))
	if username == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户名不能为空"})
		return
	}
	record, found, err := d.getMFARecord(r, username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取 MFA 状态失败"})
		return
	}
	passkeys, err := d.loadUserPasskeys(r.Context(), username)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取 MFA 状态失败"})
		return
	}
	passkeyCount := len(passkeys.Keys)
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{
		"otp":     map[string]any{"enabled": found && record.Enabled},
		"passkey": map[string]any{"enabled": passkeyCount > 0, "count": passkeyCount},
	}})
}

func (d Dependencies) getMFARecord(r *http.Request, username string) (mfaRecord, bool, error) {
	record := mfaRecord{}
	if d.Store == nil {
		return record, false, nil
	}
	found, err := d.Store.GetSetting(r.Context(), mfaSettingKey(username), &record)
	return record, found, err
}

func (d Dependencies) setMFARecord(r *http.Request, username string, record mfaRecord) error {
	if d.Store == nil {
		return nil
	}
	return d.Store.SetSetting(r.Context(), mfaSettingKey(username), record, true)
}

// readMFAInput 一次性读取 query 与 JSON body，避免同一请求体被读两次。
func readMFAInput(r *http.Request) mfaInput {
	input := mfaInput{
		Username: strings.TrimSpace(r.URL.Query().Get("username")),
		Code:     strings.TrimSpace(r.URL.Query().Get("code")),
	}
	var body struct {
		Username string `json:"username"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		if input.Username == "" {
			input.Username = strings.TrimSpace(body.Username)
		}
		if input.Code == "" {
			input.Code = strings.TrimSpace(body.Code)
		}
	}
	return input
}

type mfaInput struct {
	Username string
	Code     string
}

func mfaNewSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func mfaValidateCode(secret, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	for _, delta := range []int64{-1, 0, 1} {
		if mfaCodeAt(secret, now.Unix()/30+delta) == code {
			return true
		}
	}
	return false
}

func mfaCodeAt(secret string, counter int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	// 空种子解出来是长度为 0 的 key，HMAC 仍会算出一个「看起来正常」的口令。
	// 那意味着一旦某条 MFA 记录里的种子为空，任何人都能算出通过校验的口令，
	// 所以这里必须显式拒绝，而不是让它退化成空 key 的 HMAC。
	if err != nil || len(key) == 0 {
		return ""
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) | (uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", value%1000000)
}
