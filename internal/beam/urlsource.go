package beam

import (
	"context"
	"errors"
	"io"
	"sync"
)

// prepareRetainedURLMedia gives an HTTP body its own playback lifetime. Fetching
// follows the preparation context; successful acquisition detaches from that
// context so completing preparation does not abort the receiver's stream.
func (m *Manager) prepareRetainedURLMedia(ctx context.Context, sourceURL string) (any, string, io.Closer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := m.retryAttempts
	if attempts <= 0 {
		attempts = 1
	}
	baseBackoff := max(m.retryBaseBackoff, 0)
	maxBackoff := max(m.retryMaxBackoff, baseBackoff)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, "", nil, err
		}
		if m.torrentContext != nil && m.torrentContext.Err() != nil {
			return nil, "", nil, m.torrentContext.Err()
		}
		sourceCtx, cancelSource := context.WithCancel(context.WithoutCancel(ctx))
		retained := &urlSourceCloser{cancel: cancelSource}
		if m.torrentContext != nil {
			retained.stopShutdown = context.AfterFunc(m.torrentContext, cancelSource)
		}
		stopRequest := context.AfterFunc(ctx, cancelSource)
		// This call already accepts cancellation. Run it synchronously so no
		// worker can deliver a body after error cleanup has finished.
		media, mediaType, err := m.prepareURLMedia(sourceCtx, sourceURL)
		retained.body = asCloser(media)
		stopped := stopRequest()
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if sourceCtx.Err() != nil {
			err = sourceCtx.Err()
		} else if !stopped {
			// The preparation watcher has already started cancellation. Such a
			// body cannot be handed to a live playback session.
			err = context.Canceled
		}
		if err == nil {
			return media, mediaType, retained, nil
		}
		_ = retained.Close()
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || attempt >= attempts || !isTransientNetworkError(err) {
			return nil, "", nil, err
		}
		if err := waitForBackoff(ctx, backoffForAttempt(baseBackoff, maxBackoff, attempt)); err != nil {
			return nil, "", nil, err
		}
	}
	return nil, "", nil, lastErr
}

type urlSourceCloser struct {
	cancel       context.CancelFunc
	body         io.Closer
	stopShutdown func() bool
	once         sync.Once
	err          error
}

func (c *urlSourceCloser) Close() error {
	c.once.Do(func() {
		if c.stopShutdown != nil {
			c.stopShutdown()
		}
		c.cancel()
		if c.body != nil {
			c.err = c.body.Close()
		}
	})
	return c.err
}
