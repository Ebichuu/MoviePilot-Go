package organization

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxFileOutcomeErrorBytes = 2048

// FileOutcomeState is the terminal domain state of one organization file.
type FileOutcomeState string

const (
	// FileOutcomeCompleted means the file was organized successfully.
	FileOutcomeCompleted FileOutcomeState = "completed"
	// FileOutcomeTerminalFailed means the file reached a non-retryable failure.
	FileOutcomeTerminalFailed FileOutcomeState = "terminal_failed"
)

var (
	// ErrInvalidTerminalResults reports malformed snapshots or file outcomes.
	ErrInvalidTerminalResults = errors.New("invalid organization terminal results")
)

// FileOutcome is the path-free terminal state projected by persistence.
type FileOutcome struct {
	Ordinal int
	State   FileOutcomeState
	Error   string
}

// SnapshotContext is the immutable per-group context needed for one failed
// file. It intentionally excludes the group's other files so all-failure
// projection remains linear in memory.
type SnapshotContext struct {
	BatchID    uuid.UUID
	IntentID   uuid.UUID
	GroupKey   string
	Version    int
	Legacy     bool
	Username   string
	NeedNotify bool
	Manual     bool
	Background bool
	Media      MediaContext
}

// Clone returns an independent snapshot context.
func (c SnapshotContext) Clone() SnapshotContext {
	c.Media = c.Media.Clone()
	return c
}

// ResultSnapshot is one non-empty successful file subset and the V2 error
// text determined by the highest ordinal in the original group.
type ResultSnapshot struct {
	Snapshot        SuccessSnapshot
	ErrMsg          string
	TerminalOrdinal int
}

// Clone returns a result that shares no mutable file slices with the receiver.
func (r ResultSnapshot) Clone() ResultSnapshot {
	r.Snapshot = r.Snapshot.Clone()
	return r
}

// FileFailure is one terminal failure with its complete immutable display
// context and the corresponding file fact.
type FileFailure struct {
	Snapshot SnapshotContext
	File     FileFact
	Error    string
}

// Clone returns a failure that shares no mutable episode slice with the receiver.
func (f FileFailure) Clone() FileFailure {
	f.Snapshot = f.Snapshot.Clone()
	f.File = f.File.Clone()
	return f
}

// ProjectTerminalResults validates a complete one-to-one terminal outcome set,
// then projects non-empty success groups and per-file failures without mutating
// its inputs. Result groups preserve snapshot order; failures are ordinal-sorted.
func ProjectTerminalResults(
	snapshots []SuccessSnapshot,
	outcomes []FileOutcome,
) ([]ResultSnapshot, []FileFailure, error) {
	normalized, facts, err := normalizeTerminalSnapshots(snapshots)
	if err != nil {
		return nil, nil, err
	}
	outcomeByOrdinal, err := normalizeFileOutcomes(outcomes, facts)
	if err != nil {
		return nil, nil, err
	}

	results := make([]ResultSnapshot, 0, len(normalized))
	failures := make([]FileFailure, 0)
	for _, snapshot := range normalized {
		projected := snapshot.Clone()
		projected.Files = make([]FileFact, 0, len(snapshot.Files))
		lastOrdinal := -1
		var lastOutcome FileOutcome
		context := snapshotContext(snapshot)
		for _, file := range snapshot.Files {
			outcome := outcomeByOrdinal[file.Ordinal]
			if file.Ordinal > lastOrdinal {
				lastOrdinal = file.Ordinal
				lastOutcome = outcome
			}
			switch outcome.State {
			case FileOutcomeCompleted:
				projected.Files = append(projected.Files, file.Clone())
			case FileOutcomeTerminalFailed:
				failures = append(failures, FileFailure{
					Snapshot: context.Clone(),
					File:     file.Clone(),
					Error:    outcome.Error,
				})
			}
		}
		if len(projected.Files) == 0 {
			continue
		}
		projected, err = NormalizeSuccessSnapshot(projected)
		if err != nil {
			return nil, nil, terminalResultsError("projected snapshot")
		}
		result := ResultSnapshot{Snapshot: projected, TerminalOrdinal: lastOrdinal}
		if lastOutcome.State == FileOutcomeTerminalFailed {
			result.ErrMsg = lastOutcome.Error
		}
		results = append(results, result)
	}
	sort.Slice(failures, func(i, j int) bool {
		return failures[i].File.Ordinal < failures[j].File.Ordinal
	})
	return results, failures, nil
}

func normalizeTerminalSnapshots(
	snapshots []SuccessSnapshot,
) ([]SuccessSnapshot, map[int]FileFact, error) {
	if len(snapshots) == 0 || len(snapshots) > maxFiles {
		return nil, nil, terminalResultsError("snapshot count")
	}
	normalized := make([]SuccessSnapshot, len(snapshots))
	facts := make(map[int]FileFact)
	groups := make(map[string]struct{}, len(snapshots))
	var batchID, intentID uuid.UUID
	var username string
	var version int
	var legacy bool
	for index, snapshot := range snapshots {
		item, err := NormalizeSuccessSnapshot(snapshot)
		if err != nil || len(item.Files) == 0 {
			return nil, nil, terminalResultsError("snapshot")
		}
		if index == 0 {
			batchID, intentID, username = item.BatchID, item.IntentID, item.Username
			version, legacy = item.Version, item.Legacy
		} else if item.BatchID != batchID || item.IntentID != intentID ||
			item.Username != username || item.Version != version || item.Legacy != legacy {
			return nil, nil, terminalResultsError("snapshot identity")
		}
		if _, exists := groups[item.GroupKey]; exists {
			return nil, nil, terminalResultsError("duplicate group")
		}
		groups[item.GroupKey] = struct{}{}
		for _, file := range item.Files {
			if _, exists := facts[file.Ordinal]; exists {
				return nil, nil, terminalResultsError("duplicate ordinal")
			}
			if len(facts) == maxFiles {
				return nil, nil, terminalResultsError("file count")
			}
			facts[file.Ordinal] = file.Clone()
		}
		normalized[index] = item
	}
	if len(facts) == 0 {
		return nil, nil, terminalResultsError("file count")
	}
	return normalized, facts, nil
}

func normalizeFileOutcomes(
	outcomes []FileOutcome,
	facts map[int]FileFact,
) (map[int]FileOutcome, error) {
	if len(outcomes) != len(facts) {
		return nil, terminalResultsError("outcome coverage")
	}
	result := make(map[int]FileOutcome, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Ordinal < 0 {
			return nil, terminalResultsError("outcome ordinal")
		}
		if _, exists := facts[outcome.Ordinal]; !exists {
			return nil, terminalResultsError("unknown outcome ordinal")
		}
		if _, exists := result[outcome.Ordinal]; exists {
			return nil, terminalResultsError("duplicate outcome ordinal")
		}
		switch outcome.State {
		case FileOutcomeCompleted:
			if outcome.Error != "" {
				return nil, terminalResultsError("successful outcome error")
			}
		case FileOutcomeTerminalFailed:
			if !validFileOutcomeError(outcome.Error) {
				return nil, terminalResultsError("failed outcome error")
			}
		default:
			return nil, terminalResultsError("outcome state")
		}
		result[outcome.Ordinal] = outcome
	}
	return result, nil
}

func snapshotContext(snapshot SuccessSnapshot) SnapshotContext {
	return SnapshotContext{
		BatchID: snapshot.BatchID, IntentID: snapshot.IntentID,
		GroupKey: snapshot.GroupKey, Version: snapshot.Version, Legacy: snapshot.Legacy,
		Username: snapshot.Username, NeedNotify: snapshot.NeedNotify,
		Manual: snapshot.Manual, Background: snapshot.Background,
		Media: snapshot.Media.Clone(),
	}
}

func validFileOutcomeError(value string) bool {
	if value == "" || len(value) > maxFileOutcomeErrorBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func terminalResultsError(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalidTerminalResults, field)
}
