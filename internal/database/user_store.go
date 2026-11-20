package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// EnsureBootstrapUser 仅在指定用户不存在时创建初始管理员。
func (s *Store) EnsureBootstrapUser(ctx context.Context, username, password string) error {
	if strings.TrimSpace(password) == "" {
		return nil
	}
	var exists bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)`, username).Scan(&exists); err != nil {
		return fmt.Errorf("检查初始用户: %w", err)
	}
	if exists {
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO users (username, password_hash, is_superuser, permissions)
		VALUES ($1, $2, TRUE, '{}'::jsonb)
		ON CONFLICT (username) DO NOTHING`, username, hash)
	if err != nil {
		return fmt.Errorf("创建初始用户: %w", err)
	}
	return nil
}

// UserByName 按用户名读取鉴权用户。
func (s *Store) UserByName(ctx context.Context, username string) (auth.User, error) {
	return s.scanUser(s.db.Pool.QueryRow(ctx, `
		SELECT id, username, password_hash, is_superuser, avatar, permissions
		FROM users WHERE username=$1`, username))
}

// UserByID 按主键读取鉴权用户。
func (s *Store) UserByID(ctx context.Context, id int64) (auth.User, error) {
	return s.scanUser(s.db.Pool.QueryRow(ctx, `
		SELECT id, username, password_hash, is_superuser, avatar, permissions
		FROM users WHERE id=$1`, id))
}

// ListUsers 返回全部用户，按主键排序，对应 V2 `GET /user/`（仅超级管理员）。
func (s *Store) ListUsers(ctx context.Context) ([]auth.User, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, username, password_hash, is_superuser, avatar, permissions
		FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表: %w", err)
	}
	defer rows.Close()
	users := make([]auth.User, 0, 8)
	for rows.Next() {
		user, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取用户列表: %w", err)
	}
	return users, nil
}

// CreateUser 新增用户，对应 V2 `POST /user/`：密码由调用方用 auth.HashPassword 预先
// 哈希，权限为空时写入空对象，避免前端读到 null。
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, superUser bool, permissions map[string]any) (auth.User, error) {
	encoded, err := encodePermissions(permissions)
	if err != nil {
		return auth.User{}, err
	}
	var id int64
	if err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, is_superuser, permissions)
		VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
		username, passwordHash, superUser, encoded).Scan(&id); err != nil {
		return auth.User{}, fmt.Errorf("创建用户: %w", err)
	}
	return s.UserByID(ctx, id)
}

// UpdateUser 更新用户，对应 V2 `PUT /user/`；passwordHash 为空表示保持原密码。
func (s *Store) UpdateUser(ctx context.Context, id int64, username, passwordHash string, superUser bool, permissions map[string]any) (auth.User, error) {
	encoded, err := encodePermissions(permissions)
	if err != nil {
		return auth.User{}, err
	}
	if strings.TrimSpace(passwordHash) == "" {
		_, err = s.db.Pool.Exec(ctx, `
			UPDATE users SET username=$2, is_superuser=$3, permissions=$4::jsonb, updated_at=now()
			WHERE id=$1`, id, username, superUser, encoded)
	} else {
		_, err = s.db.Pool.Exec(ctx, `
			UPDATE users SET username=$2, password_hash=$3, is_superuser=$4, permissions=$5::jsonb, updated_at=now()
			WHERE id=$1`, id, username, passwordHash, superUser, encoded)
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("更新用户: %w", err)
	}
	return s.UserByID(ctx, id)
}

// UpdateUserAvatar 写入头像 data URL，对应 V2 `POST /user/avatar/{user_id}`。
func (s *Store) UpdateUserAvatar(ctx context.Context, id int64, avatar string) error {
	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE users SET avatar=$2, updated_at=now() WHERE id=$1`, id, avatar); err != nil {
		return fmt.Errorf("更新用户头像: %w", err)
	}
	return nil
}

// DeleteUser 删除用户，对应 V2 `DELETE /user/id/{user_id}` 与 `/user/name/{user_name}`。
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id); err != nil {
		return fmt.Errorf("删除用户: %w", err)
	}
	return nil
}

// CountSuperUsers 用于阻止删掉最后一位超级管理员。
func (s *Store) CountSuperUsers(ctx context.Context) (int, error) {
	var count int
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE is_superuser`).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计超级管理员: %w", err)
	}
	return count, nil
}

func encodePermissions(permissions map[string]any) ([]byte, error) {
	if permissions == nil {
		permissions = map[string]any{}
	}
	encoded, err := json.Marshal(permissions)
	if err != nil {
		return nil, fmt.Errorf("编码用户权限: %w", err)
	}
	return encoded, nil
}

func (s *Store) scanUser(row rowScanner) (auth.User, error) {
	var user auth.User
	var permissions []byte
	if err := row.Scan(&user.ID, &user.Username, &user.Password, &user.SuperUser, &user.Avatar, &permissions); err != nil {
		return auth.User{}, err
	}
	if err := json.Unmarshal(permissions, &user.Permissions); err != nil {
		return auth.User{}, fmt.Errorf("解析用户权限: %w", err)
	}
	return user, nil
}
