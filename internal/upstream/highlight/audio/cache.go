package audio

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type episodePreparation struct {
	done     chan struct{}
	prepared PreparedAudio
	err      error
}

// EpisodeCache shares one preparation (including its failure) among all
// consumers of the same episode request. It is safe for concurrent candidates.
type EpisodeCache struct {
	preparer AudioPreparer
	mu       sync.Mutex
	entries  map[episodeCacheKey]*episodePreparation
}

type episodeCacheKey struct {
	sourcePath string
	tempDir    string
	episode    int
	format     PCMFormat
}

var _ AudioPreparer = (*EpisodeCache)(nil)

func NewEpisodeCache(preparer AudioPreparer) *EpisodeCache {
	return &EpisodeCache{preparer: preparer, entries: make(map[episodeCacheKey]*episodePreparation)}
}

func (c *EpisodeCache) Prepare(ctx context.Context, request AudioPrepareRequest) (PreparedAudio, error) {
	episodeNumber := request.EpisodeNumber
	normalized, err := request.normalized()
	if err != nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: episodeNumber, Kind: FailureRequest, Err: err}
	}
	if c == nil || c.preparer == nil {
		return PreparedAudio{}, &PreparationError{EpisodeNumber: request.EpisodeNumber, Kind: FailureRequest, Err: fmt.Errorf("%w: audio preparer is not configured", ErrInvalidRequest)}
	}
	if err := ctx.Err(); err != nil {
		return PreparedAudio{}, err
	}
	key := episodeCacheKey{
		sourcePath: normalized.SourceVideoPath,
		tempDir:    normalized.TaskTempDir,
		episode:    normalized.EpisodeNumber,
		format:     normalized.Format,
	}

	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[episodeCacheKey]*episodePreparation)
		}
		if entry := c.entries[key]; entry != nil {
			c.mu.Unlock()
			select {
			case <-entry.done:
				if callerCancellation(entry.err) {
					if err := ctx.Err(); err != nil {
						return PreparedAudio{}, err
					}
					continue
				}
				return entry.prepared, entry.err
			case <-ctx.Done():
				return PreparedAudio{}, ctx.Err()
			}
		}
		entry := &episodePreparation{done: make(chan struct{})}
		c.entries[key] = entry
		c.mu.Unlock()

		prepared, prepareErr := c.preparer.Prepare(ctx, normalized)
		c.mu.Lock()
		entry.prepared, entry.err = prepared, prepareErr
		if callerCancellation(prepareErr) && c.entries[key] == entry {
			delete(c.entries, key)
		}
		close(entry.done)
		c.mu.Unlock()
		return prepared, prepareErr
	}
}

func callerCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
