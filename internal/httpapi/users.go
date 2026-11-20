package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// avatarMaxBytes 限制头像大小，避免把数据库塞满 base64 大图。
const avatarMaxBytes = 2 << 20

type userCreateRequest struct {
	Name        string         `json:"name"`
	Password    string         `json:"password"`
	IsSuperUser bool           `json:"is_superuser"`
	Permissions map[string]any `json:"permissions"`
	Settings    map[string]any `json:"settings"`
}

type userUpdateRequest struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Password    string         `json:"password"`
	IsSuperUser bool           `json:"is_superuser"`
	Permissions map[string]any `json:"permissions"`
	Settings    map[string]any `json:"settings"`
}

// listUsers 对应 V2 `GET /user/`：仅超级管理员可读取全部用户。
func (d Dependencies) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := d.Store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询用户失败")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// createUser 对应 V2 `POST /user/`：用户名唯一、密码按 V2 的强度规则校验。
func (d Dependencies) createUser(w http.ResponseWriter, r *http.Request) {
	var request userCreateRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误"})
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户名不能为空"})
		return
	}
	if _, err := d.Store.UserByName(r.Context(), name); err == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户已存在"})
		return
	}
	if !validUserPassword(request.Password) {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: userPasswordRule})
		return
	}
	hash, err := auth.HashPassword(request.Password)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "密码处理失败"})
		return
	}
	if _, err := d.Store.CreateUser(r.Context(), name, hash, request.IsSuperUser, request.Permissions); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "创建用户失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// updateUser 对应 V2 `PUT /user/`：密码留空表示不修改。
func (d Dependencies) updateUser(w http.ResponseWriter, r *http.Request) {
	var request userUpdateRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "参数错误"})
		return
	}
	if request.ID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户ID不能为空"})
		return
	}
	current, err := d.Store.UserByID(r.Context(), request.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户不存在"})
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = current.Username
	}
	if name != current.Username {
		if _, err := d.Store.UserByName(r.Context(), name); err == nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户已存在"})
			return
		}
	}
	hash := ""
	if strings.TrimSpace(request.Password) != "" {
		if !validUserPassword(request.Password) {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: userPasswordRule})
			return
		}
		hash, err = auth.HashPassword(request.Password)
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "密码处理失败"})
			return
		}
	}
	permissions := request.Permissions
	if permissions == nil {
		permissions = current.Permissions
	}
	if _, err := d.Store.UpdateUser(r.Context(), request.ID, name, hash, request.IsSuperUser, permissions); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "更新用户失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// getUser 对应 V2 `GET /user/{username}`：用户详情，仅超级管理员可读。
func (d Dependencies) getUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "username"))
	user, err := d.Store.UserByName(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusNotFound, "用户不存在")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// deleteUserByID 对应 V2 `DELETE /user/id/{user_id}`。
func (d Dependencies) deleteUserByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "user_id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户ID无效"})
		return
	}
	user, err := d.Store.UserByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户不存在"})
		return
	}
	d.deleteUser(w, r, user)
}

// deleteUserByName 对应 V2 `DELETE /user/name/{user_name}`。
func (d Dependencies) deleteUserByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "user_name"))
	user, err := d.Store.UserByName(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户不存在"})
		return
	}
	d.deleteUser(w, r, user)
}

func (d Dependencies) deleteUser(w http.ResponseWriter, r *http.Request, user auth.User) {
	if user.SuperUser {
		count, err := d.Store.CountSuperUsers(r.Context())
		if err != nil {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "查询用户失败"})
			return
		}
		if count <= 1 {
			writeJSON(w, http.StatusOK, Response{Success: false, Message: "不能删除最后一个超级管理员"})
			return
		}
	}
	if err := d.Store.DeleteUser(r.Context(), user.ID); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "删除用户失败"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// uploadUserAvatar 对应 V2 `POST /user/avatar/{user_id}`：把上传的图片转成 base64
// data URL 存入用户表，与 V2 的存储方式一致（前端直接渲染 avatar 字段）。
func (d Dependencies) uploadUserAvatar(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "用户身份无效")
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, "user_id")), 10, 64)
	if err != nil || userID <= 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户ID无效"})
		return
	}
	current, err := d.Store.UserByName(r.Context(), claims.Username)
	if err != nil {
		writeError(w, http.StatusForbidden, "用户身份无效")
		return
	}
	if current.ID != userID && !claims.SuperUser {
		writeError(w, http.StatusForbidden, "用户权限不足")
		return
	}
	if _, err := d.Store.UserByID(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "用户不存在"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "缺少上传文件"})
		return
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, avatarMaxBytes))
	if err != nil || len(content) == 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "读取上传文件失败"})
		return
	}
	avatar := "data:image/ico;base64," + base64.StdEncoding.EncodeToString(content)
	if err := d.Store.UpdateUserAvatar(r.Context(), userID, avatar); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "保存头像失败"})
		return
	}
	name := ""
	if header != nil {
		name = header.Filename
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"filename": name}})
}

// userPasswordRule 复刻 V2 的提示文案：密码需包含字母、数字、特殊字符中的至少两种。
const userPasswordRule = "密码需包含字母、数字、特殊字符中的至少两种，长度 8-50"

// validUserPassword 复刻 V2 正则
// `^(?![a-zA-Z]+$)(?!\d+$)(?![^\da-zA-Z\s]+$).{6,50}$`：长度 6-50、不能只由单一类别
// 组成。下限取 8，与本地 auth.HashPassword 的 bcrypt 长度下限保持一致。
func validUserPassword(password string) bool {
	runes := []rune(password)
	if len(runes) < 8 || len(runes) > 50 {
		return false
	}
	var hasLetter, hasDigit, hasSpecial bool
	for _, char := range runes {
		switch {
		case unicode.IsLetter(char):
			hasLetter = true
		case unicode.IsDigit(char):
			hasDigit = true
		case unicode.IsSpace(char):
		default:
			hasSpecial = true
		}
	}
	categories := 0
	for _, present := range []bool{hasLetter, hasDigit, hasSpecial} {
		if present {
			categories++
		}
	}
	return categories >= 2
}

// decodeJSONBody 读取上限 1MB 的 JSON 请求体。
func decodeJSONBody(r *http.Request, target any) error {
	if r == nil || r.Body == nil {
		return errors.New("缺少请求体")
	}
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target)
}
