// Package scrapeflow owns durable NFO planning and execution rules.
package scrapeflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// MaxAttempts is the hard retry bound for one NFO request.
	MaxAttempts = 5
	// SweepPageSize bounds one recovery scan.
	SweepPageSize = 100
)

// Kind identifies the NFO document to generate.
type Kind string

const (
	KindMovie   Kind = "movie"
	KindTVShow  Kind = "tvshow"
	KindEpisode Kind = "episode"
	KindUnknown Kind = "unknown"
)

// State is the durable request state.
type State string

const (
	StatePending   State = "pending"
	StateWorking   State = "working"
	StateFailed    State = "failed"
	StateCompleted State = "completed"
	StateSkipped   State = "skipped"
)

// Episode is one season/episode coordinate bound to a download intent.
type Episode struct {
	Season  int
	Episode int
}

// Source is the media identity needed to derive a scrape plan.
type Source struct {
	Username       string
	SubscriptionID int64
	MediaType      string
	MediaSource    string
	MediaID        string
	Season         int
	Episodes       []Episode
}

// RequestPlan is one immutable NFO side effect.
type RequestPlan struct {
	ID         uuid.UUID
	Kind       Kind
	MediaPath  string
	TMDBID     int
	Season     int
	Episode    int
	SkipReason string
}

// Valid reports whether the immutable request can be persisted safely.
func (p RequestPlan) Valid() bool {
	if p.ID == uuid.Nil || !validKind(p.Kind) || strings.TrimSpace(p.MediaPath) != p.MediaPath ||
		p.MediaPath == "" || len(p.MediaPath) > 4096 || !filepath.IsAbs(p.MediaPath) ||
		strings.IndexByte(p.MediaPath, 0) >= 0 || p.Season < 0 || p.Season > 999 ||
		p.Episode < 0 || p.Episode > 5000 || len(p.SkipReason) > 128 {
		return false
	}
	if p.SkipReason != "" {
		return p.TMDBID == 0
	}
	if p.TMDBID < 1 {
		return false
	}
	switch p.Kind {
	case KindMovie, KindTVShow:
		return p.Season == 0 && p.Episode == 0
	case KindEpisode:
		return p.Season >= 0 && p.Episode > 0
	default:
		return false
	}
}

// Plan is the complete immutable NFO snapshot for one organization batch.
type Plan struct {
	BatchID  uuid.UUID
	IntentID uuid.UUID
	Username string
	Requests []RequestPlan
}

// Clone returns a detached copy suitable for persistence or tests.
func (p Plan) Clone() Plan {
	p.Requests = append([]RequestPlan(nil), p.Requests...)
	return p
}

// Valid reports whether all request identities belong to this batch.
func (p Plan) Valid() bool {
	if p.BatchID == uuid.Nil || p.IntentID == uuid.Nil || strings.TrimSpace(p.Username) == "" ||
		strings.TrimSpace(p.Username) != p.Username || len(p.Requests) > 10_000 {
		return false
	}
	seen := make(map[uuid.UUID]struct{}, len(p.Requests))
	for _, request := range p.Requests {
		if !request.Valid() {
			return false
		}
		if _, exists := seen[request.ID]; exists {
			return false
		}
		seen[request.ID] = struct{}{}
	}
	return true
}

// Request is the durable projection consumed by one worker.
type Request struct {
	ID           uuid.UUID
	Kind         Kind
	MediaPath    string
	TMDBID       int
	Season       int
	Episode      int
	State        State
	AttemptCount int
	NextAttempt  time.Time
}

// Valid reports whether a claimed request is executable.
func (r Request) Valid(id uuid.UUID) bool {
	return r.ID == id && r.AttemptCount >= 1 && r.AttemptCount <= MaxAttempts &&
		(RequestPlan{ID: r.ID, Kind: r.Kind, MediaPath: r.MediaPath, TMDBID: r.TMDBID,
			Season: r.Season, Episode: r.Episode}).Valid()
}

// Lease is the token-CAS ownership returned by persistence.
type Lease struct {
	Request Request
	Token   uuid.UUID
}

// Valid reports whether the lease belongs to the requested row.
func (l Lease) Valid(id uuid.UUID) bool {
	return l.Token != uuid.Nil && l.Request.Valid(id)
}

// SourceStore loads media identity without exposing persistence types.
type SourceStore interface {
	LoadScrapeSource(context.Context, uuid.UUID) (Source, error)
}

// Store owns one request lease and its terminal or retryable transition.
type Store interface {
	ClaimScrape(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error)
	CompleteScrape(context.Context, uuid.UUID, uuid.UUID, time.Time) error
	RescheduleScrape(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error
}

// SweepStore owns bounded recovery discovery.
type SweepStore interface {
	RecoverExpiredScrapes(context.Context, time.Time, int) (int64, error)
	ListDueScrapes(context.Context, time.Time, int) ([]uuid.UUID, error)
}

// Scraper generates one concrete NFO document.
type Scraper interface {
	ScrapeMovieNFO(context.Context, int, string) error
	ScrapeTVShowNFO(context.Context, int, string) error
	ScrapeEpisodeNFO(context.Context, int, int, int, string) error
}

// Enqueuer requests execution of a persisted NFO row.
type Enqueuer interface {
	EnqueueScrape(context.Context, uuid.UUID) error
}

var (
	ErrInvalidPlan = errors.New("NFO 计划无效")
	ErrPersistence = errors.New("NFO 状态持久化失败")
	ErrExecution   = errors.New("NFO 生成失败")
)

func validKind(kind Kind) bool {
	switch kind {
	case KindMovie, KindTVShow, KindEpisode, KindUnknown:
		return true
	default:
		return false
	}
}
