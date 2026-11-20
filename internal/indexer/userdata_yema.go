package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
)

func (a *YemaAdapter) FetchSiteUserData(ctx context.Context) (SiteUserData, error) {
	result := newSiteUserData(a.base)
	endpoint, err := resolveSiteURL(a.base, "/openApi/user/fetchBasicInfo.json")
	if err != nil {
		return SiteUserData{}, err
	}
	headers := a.headers.Clone()
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("Content-Type", "application/json")
	body, err := a.fetchYemaUserDataPage(ctx, endpoint, headers)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return SiteUserData{}, err
		}
		result.ErrMsg = userDataString("获取用户信息失败，未收到开放 API 响应")
		return result, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		result.ErrMsg = userDataString("获取用户信息失败，未收到开放 API 响应")
		return result, nil
	}

	payload, err := decodeYemaUserDataJSON(body)
	if err != nil {
		result.ErrMsg = userDataString("获取用户信息失败，开放 API 响应不是有效 JSON")
		return result, nil
	}
	document, ok := payload.(map[string]any)
	if !ok {
		result.ErrMsg = userDataString("获取用户信息失败，开放 API 响应结构无效")
		return result, nil
	}
	userInfo, dataOK := document["data"].(map[string]any)
	success, successOK := document["success"].(bool)
	if !successOK || !success || !dataOK {
		message := valueString(document["errorMessage"])
		if message == "" {
			message = "获取用户信息失败"
		}
		result.ErrMsg = &message
		return result, nil
	}

	result.UserID = userDataID(userInfo["id"])
	result.Username = userDataString(userInfo["name"])
	result.UserLevel = userDataString(userInfo["level"])
	result.JoinAt = userDataDateTime(userInfo["registerTime"])
	result.Upload = userDataInt64(userInfo["promotionUploadSize"])
	result.Download = userDataInt64(userInfo["promotionDownloadSize"])
	result.Ratio = math.Round(float64(result.Upload)/float64(max(result.Download, 1))*100) / 100
	result.Bonus = userDataFloat(userInfo["bonus"])
	return result, nil
}

func (a *YemaAdapter) fetchYemaUserDataPage(ctx context.Context, endpoint string, headers http.Header) ([]byte, error) {
	response, err := a.client.doResponseLimited(
		ctx, http.MethodPost, endpoint, headers, []byte("{}"), a.client.maxBodyBytes, true,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusForbidden, http.StatusInternalServerError:
		return response.Body, nil
	default:
		return nil, nil
	}
}

func decodeYemaUserDataJSON(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("YemaPT 用户数据响应包含多余内容")
	}
	return payload, nil
}
