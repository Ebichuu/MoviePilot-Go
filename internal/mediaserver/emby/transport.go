package emby

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxGETAttempts    = 3
	maximumRetryDelay = 2 * time.Second
)

type rawResponse struct {
	statusCode int
	body       []byte
	retryAfter string
}

func (c *Client) doOnce(ctx context.Context, method, endpoint string, maxBodyBytes int64) (rawResponse, bool, *Error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return rawResponse{}, false, newError(ErrorCodeInvalidArgument, 0, nil)
	}
	setRequestHeaders(request, c.token, c.authorization)
	if err := c.acquire(ctx); err != nil {
		return rawResponse{}, false, err
	}
	defer c.release()

	response, requestErr := c.httpClient.Do(request)
	if requestErr != nil {
		return rawResponse{}, true, classifyRequestError(ctx, requestErr)
	}
	defer response.Body.Close()
	return readResponse(ctx, response, maxBodyBytes)
}

func setRequestHeaders(request *http.Request, token, authorization string) {
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MoviePilot-Go/0.1")
	request.Header.Set("X-Emby-Token", token)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
}

func (c *Client) acquire(ctx context.Context) *Error {
	select {
	case c.permits <- struct{}{}:
		return nil
	case <-ctx.Done():
		return canceledError(ctx.Err())
	}
}

func (c *Client) release() {
	<-c.permits
}

func classifyRequestError(ctx context.Context, err error) *Error {
	if isRedirectError(err) {
		return newError(ErrorCodeRedirect, 0, nil)
	}
	if ctxErr := contextCause(ctx, err); ctxErr != nil {
		return canceledError(ctxErr)
	}
	result := newError(ErrorCodeTransport, 0, nil)
	result.temporary = temporaryTransportError(err)
	return result
}

func readResponse(ctx context.Context, response *http.Response, maxBodyBytes int64) (rawResponse, bool, *Error) {
	result := rawResponse{
		statusCode: response.StatusCode,
		retryAfter: response.Header.Get("Retry-After"),
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, readErr := readLimited(response.Body, maxErrorBodyBytes)
		if readErr != nil && ctx.Err() != nil {
			return rawResponse{}, true, canceledError(ctx.Err())
		}
		return result, true, nil
	}

	body, readErr := readLimited(response.Body, maxBodyBytes)
	if readErr != nil {
		return rawResponse{}, true, classifyResponseReadError(ctx, response.StatusCode, readErr)
	}
	result.body = body
	return result, true, nil
}

func classifyResponseReadError(ctx context.Context, statusCode int, err error) *Error {
	if errors.Is(err, errBodyTooLarge) {
		return newError(ErrorCodeResponseTooLarge, statusCode, nil)
	}
	if ctxErr := contextCause(ctx, err); ctxErr != nil {
		return &Error{Code: ErrorCodeCanceled, StatusCode: statusCode, cause: ctxErr}
	}
	result := newError(ErrorCodeTransport, statusCode, nil)
	result.temporary = temporaryTransportError(err)
	return result
}

var errBodyTooLarge = errors.New("emby response body too large")

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if seconds, err := strconv.ParseInt(strings.TrimSpace(retryAfter), 10, 64); err == nil && seconds >= 0 {
			if seconds >= int64(maximumRetryDelay/time.Second) {
				return maximumRetryDelay
			}
			return minDuration(time.Duration(seconds)*time.Second, maximumRetryDelay)
		}
		if when, err := http.ParseTime(retryAfter); err == nil {
			return minDuration(maxDuration(time.Until(when), 0), maximumRetryDelay)
		}
	}
	delay := 50 * time.Millisecond * time.Duration(1<<attempt)
	return minDuration(delay, maximumRetryDelay)
}

func waitForRetry(ctx context.Context, delay time.Duration) *Error {
	if err := ctx.Err(); err != nil {
		return canceledError(err)
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return canceledError(ctx.Err())
	}
}

func temporaryTransportError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func contextCause(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func canceledError(cause error) *Error {
	return newError(ErrorCodeCanceled, 0, cause)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}
