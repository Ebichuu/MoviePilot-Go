// Package emby provides a small, bounded HTTP client for the Emby API.
package emby

import (
	"errors"
	"fmt"
	"time"
)

const (
	maxConcurrentRequests        = 4
	maxErrorBodyBytes      int64 = 64 << 10
	maxSystemInfoBytes     int64 = 64 << 10
	maxLibrariesBytes      int64 = 256 << 10
	maxItemsBytes          int64 = 512 << 10
	maxRefreshBytes        int64 = 64 << 10
	maxHeaderBytes               = 256 << 10
	maxFindItemsLimit            = 200
	maxFindItemsStartIndex       = 10_000
	maxMovieSearchLimit          = 10
	maxSeriesSearchLimit         = 10
	defaultTimeout               = 15 * time.Second
)

var (
	errCrossOriginRedirect = errors.New("emby cross-origin redirect")
	errHTTPSDowngrade      = errors.New("emby HTTPS downgrade")
	errTooManyRedirects    = errors.New("emby too many redirects")
	errRedirectMethod      = errors.New("emby redirect changed method")

	// ErrOutcomeUnknown identifies a refresh request whose response was lost
	// after the request could have reached Emby.
	ErrOutcomeUnknown = errors.New("emby refresh outcome unknown")
)

// ErrorCode is a stable, non-sensitive Emby error classification.
type ErrorCode string

const (
	ErrorCodeInvalidConfig    ErrorCode = "invalid_config"
	ErrorCodeInvalidArgument  ErrorCode = "invalid_argument"
	ErrorCodeCanceled         ErrorCode = "canceled"
	ErrorCodeTransport        ErrorCode = "transport"
	ErrorCodeRedirect         ErrorCode = "redirect_rejected"
	ErrorCodeRateLimited      ErrorCode = "rate_limited"
	ErrorCodeHTTPStatus       ErrorCode = "http_status"
	ErrorCodeResponseTooLarge ErrorCode = "response_too_large"
	ErrorCodeInvalidResponse  ErrorCode = "invalid_response"
	ErrorCodeOutcomeUnknown   ErrorCode = "outcome_unknown"
)

// Error reports a sanitized Emby failure. It never contains credentials,
// request URLs, media paths, provider identifiers, or response bodies.
type Error struct {
	Code       ErrorCode
	StatusCode int
	cause      error
	temporary  bool
}

// Error returns a stable, sanitized message.
func (e *Error) Error() string {
	if e == nil {
		return "emby: unknown error"
	}
	switch e.Code {
	case ErrorCodeInvalidConfig:
		return "emby: invalid configuration"
	case ErrorCodeInvalidArgument:
		return "emby: invalid argument"
	case ErrorCodeCanceled:
		return "emby: request canceled"
	case ErrorCodeTransport:
		return "emby: transport failure"
	case ErrorCodeRedirect:
		return "emby: redirect rejected"
	case ErrorCodeRateLimited:
		return "emby: rate limited"
	case ErrorCodeHTTPStatus:
		if e.StatusCode > 0 {
			return fmt.Sprintf("emby: HTTP %d", e.StatusCode)
		}
		return "emby: HTTP failure"
	case ErrorCodeResponseTooLarge:
		return "emby: response too large"
	case ErrorCodeInvalidResponse:
		return "emby: invalid response"
	case ErrorCodeOutcomeUnknown:
		return "emby: refresh outcome unknown"
	default:
		return "emby: unknown error"
	}
}

// Unwrap preserves context cancellation without exposing transport or response
// details.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is supports errors.Is(err, ErrOutcomeUnknown).
func (e *Error) Is(target error) bool {
	return e != nil && e.Code == ErrorCodeOutcomeUnknown && target == ErrOutcomeUnknown
}

// Config configures an Emby client.
type Config struct {
	BaseURL       string
	PlayHost      string
	Token         string
	Authorization string
	Timeout       time.Duration
}

// SystemInfo is the small server identity subset used by MoviePilot.
type SystemInfo struct {
	ID                     string `json:"Id"`
	ServerName             string `json:"ServerName"`
	Version                string `json:"Version"`
	ProductName            string `json:"ProductName"`
	OperatingSystem        string `json:"OperatingSystem"`
	LocalAddress           string `json:"LocalAddress"`
	WanAddress             string `json:"WanAddress"`
	StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
	SupportsLibraryMonitor bool   `json:"SupportsLibraryMonitor"`
}

// Library describes an Emby virtual folder.
type Library struct {
	Name               string   `json:"Name"`
	CollectionType     string   `json:"CollectionType"`
	Locations          []string `json:"Locations"`
	ItemID             string   `json:"ItemId"`
	PrimaryImageItemID string   `json:"PrimaryImageItemId"`
}

// Item is the small Emby item subset needed for media confirmation.
type Item struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	Type              string            `json:"Type"`
	Path              string            `json:"Path"`
	ParentID          string            `json:"ParentId"`
	SeriesID          string            `json:"SeriesId"`
	ServerID          string            `json:"ServerId"`
	OriginalTitle     string            `json:"OriginalTitle"`
	IndexNumber       *int              `json:"IndexNumber"`
	ParentIndexNumber *int              `json:"ParentIndexNumber"`
	ProductionYear    int               `json:"ProductionYear"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
}

// ItemsResponse is the envelope returned by Emby's /Items endpoint.
type ItemsResponse struct {
	Items            []Item `json:"Items"`
	TotalRecordCount int    `json:"TotalRecordCount"`
	StartIndex       int    `json:"StartIndex"`
}

// FindItemsOptions controls a bounded Movie/Episode lookup. A zero Limit uses
// two; callers that enumerate results must paginate and stay within the hard
// per-request and start-index limits.
type FindItemsOptions struct {
	Path                string
	AnyProviderIDEquals string
	Recursive           bool
	Fields              []string
	StartIndex          int
	Limit               int
}

// SearchSeriesOptions controls the V2-compatible bounded Series lookup used
// before enumerating a show's episodes.
type SearchSeriesOptions struct {
	SearchTerm string
	Limit      int
}

// SearchMoviesOptions controls the V2-compatible bounded Movie lookup used
// before a normal movie subscription searches for another download.
type SearchMoviesOptions struct {
	SearchTerm string
	Limit      int
}

// ListEpisodesOptions controls one bounded /Shows/{id}/Episodes page.
type ListEpisodesOptions struct {
	SeriesID   string
	StartIndex int
	Limit      int
}

func newError(code ErrorCode, statusCode int, cause error) *Error {
	return &Error{Code: code, StatusCode: statusCode, cause: cause}
}
