package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const siteUserDataColumns = `
	user_data.id, user_data.domain, user_data.name, user_data.username, user_data.userid,
	user_data.user_level, user_data.join_at, user_data.bonus, user_data.upload, user_data.download,
	user_data.ratio, user_data.seeding, user_data.leeching, user_data.seeding_size,
	user_data.leeching_size, user_data.seeding_info, user_data.message_unread,
	user_data.message_unread_contents, user_data.err_msg, user_data.updated_day, user_data.updated_time`

// SiteUserData is one V2-compatible daily site-user snapshot.
type SiteUserData struct {
	ID                    int64           `json:"id"`
	Domain                string          `json:"domain"`
	Name                  string          `json:"name"`
	Username              *string         `json:"username"`
	UserID                *string         `json:"userid"`
	UserLevel             *string         `json:"user_level"`
	JoinAt                *string         `json:"join_at"`
	Bonus                 *float64        `json:"bonus"`
	Upload                *float64        `json:"upload"`
	Download              *float64        `json:"download"`
	Ratio                 *float64        `json:"ratio"`
	Seeding               *float64        `json:"seeding"`
	Leeching              *float64        `json:"leeching"`
	SeedingSize           *float64        `json:"seeding_size"`
	LeechingSize          *float64        `json:"leeching_size"`
	SeedingInfo           json.RawMessage `json:"seeding_info"`
	MessageUnread         *int            `json:"message_unread"`
	MessageUnreadContents json.RawMessage `json:"message_unread_contents"`
	ErrMsg                *string         `json:"err_msg"`
	UpdatedDay            string          `json:"updated_day"`
	UpdatedTime           string          `json:"updated_time"`
}

// SaveSiteUserData applies V2's one-row-per-domain/day update behavior.
func (s *Store) SaveSiteUserData(
	ctx context.Context,
	domain string,
	name string,
	payload SiteUserData,
) error {
	now := time.Now()
	return s.saveSiteUserDataAt(ctx, domain, name, payload, now)
}

func (s *Store) saveSiteUserDataAt(
	ctx context.Context,
	domain string,
	name string,
	payload SiteUserData,
	now time.Time,
) error {
	payload.Domain = domain
	payload.Name = name
	payload.UpdatedDay = now.Format("2006-01-02")
	payload.UpdatedTime = now.Format("15:04:05")
	errMessage := ""
	if payload.ErrMsg != nil && *payload.ErrMsg != "" {
		errMessage = *payload.ErrMsg
	}
	payload.ErrMsg = &errMessage

	var id int64
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id
		FROM site_userdata
		WHERE domain = $1 AND updated_day = $2
		LIMIT 1`, domain, payload.UpdatedDay).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.insertSiteUserData(ctx, payload); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取站点用户数据日快照: %w", err)
	}
	if errMessage != "" {
		return nil
	}
	if err := s.updateSiteUserData(ctx, id, payload); err != nil {
		return err
	}
	return nil
}

func (s *Store) insertSiteUserData(ctx context.Context, payload SiteUserData) error {
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO site_userdata (
			domain, name, username, userid, user_level, join_at,
			bonus, upload, download, ratio, seeding, leeching, seeding_size, leeching_size,
			seeding_info, message_unread, message_unread_contents, err_msg, updated_day, updated_time
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20
		)`, siteUserDataArguments(payload)...)
	if err != nil {
		return fmt.Errorf("新增站点用户数据日快照: %w", err)
	}
	return nil
}

func (s *Store) updateSiteUserData(ctx context.Context, id int64, payload SiteUserData) error {
	arguments := append(siteUserDataArguments(payload), id)
	_, err := s.db.Pool.Exec(ctx, `
		UPDATE site_userdata
		SET domain = $1,
			name = $2,
			username = $3,
			userid = $4,
			user_level = $5,
			join_at = $6,
			bonus = $7,
			upload = $8,
			download = $9,
			ratio = $10,
			seeding = $11,
			leeching = $12,
			seeding_size = $13,
			leeching_size = $14,
			seeding_info = $15,
			message_unread = $16,
			message_unread_contents = $17,
			err_msg = $18,
			updated_day = $19,
			updated_time = $20
		WHERE id = $21`, arguments...)
	if err != nil {
		return fmt.Errorf("更新站点用户数据日快照: %w", err)
	}
	return nil
}

func siteUserDataArguments(payload SiteUserData) []any {
	return []any{
		payload.Domain, payload.Name, payload.Username, payload.UserID, payload.UserLevel, payload.JoinAt,
		payload.Bonus, payload.Upload, payload.Download, payload.Ratio, payload.Seeding, payload.Leeching,
		payload.SeedingSize, payload.LeechingSize, payload.SeedingInfo, payload.MessageUnread,
		payload.MessageUnreadContents, payload.ErrMsg, payload.UpdatedDay, payload.UpdatedTime,
	}
}

// LatestSiteUserData returns every row on each domain's latest successful day.
func (s *Store) LatestSiteUserData(ctx context.Context) ([]SiteUserData, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT `+siteUserDataColumns+`
		FROM site_userdata AS user_data
		JOIN (
			SELECT domain, MAX(updated_day) AS latest_update_day
			FROM site_userdata
			WHERE err_msg IS NULL OR err_msg = ''
			GROUP BY domain
		) AS latest_success
			ON user_data.domain = latest_success.domain
			AND user_data.updated_day = latest_success.latest_update_day
		ORDER BY user_data.updated_time DESC`)
	if err != nil {
		return nil, fmt.Errorf("读取最新站点用户数据: %w", err)
	}
	defer rows.Close()
	result, err := collectSiteUserData(rows)
	if err != nil {
		return nil, fmt.Errorf("读取最新站点用户数据: %w", err)
	}
	return result, nil
}

// SiteUserDataByDomain returns V2 history rows without adding an order.
func (s *Store) SiteUserDataByDomain(
	ctx context.Context,
	domain string,
	workdate string,
) ([]SiteUserData, error) {
	query := `SELECT ` + siteUserDataColumns + ` FROM site_userdata AS user_data WHERE user_data.domain = $1`
	arguments := []any{domain}
	if workdate != "" {
		query += ` AND user_data.updated_day = $2`
		arguments = append(arguments, workdate)
	}
	rows, err := s.db.Pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("读取站点用户数据历史: %w", err)
	}
	defer rows.Close()
	result, err := collectSiteUserData(rows)
	if err != nil {
		return nil, fmt.Errorf("读取站点用户数据历史: %w", err)
	}
	return result, nil
}

type siteUserDataRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func collectSiteUserData(rows siteUserDataRows) ([]SiteUserData, error) {
	result := make([]SiteUserData, 0)
	for rows.Next() {
		var item SiteUserData
		if err := rows.Scan(
			&item.ID, &item.Domain, &item.Name, &item.Username, &item.UserID, &item.UserLevel, &item.JoinAt,
			&item.Bonus, &item.Upload, &item.Download, &item.Ratio, &item.Seeding, &item.Leeching,
			&item.SeedingSize, &item.LeechingSize, &item.SeedingInfo, &item.MessageUnread,
			&item.MessageUnreadContents, &item.ErrMsg, &item.UpdatedDay, &item.UpdatedTime,
		); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
