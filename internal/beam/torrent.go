package beam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go2tv.app/go2tv/v2/mediasource"
	"go2tv.app/go2tv/v2/torrentstream"
	"go2tv.app/mcp-beam/internal/domain"
)

const (
	torrentMetadataTimeout = 60 * time.Second
	torrentMinTimeoutMS    = 1000
	torrentMaxTimeoutMS    = 120000
)

type torrentSession interface {
	Files(context.Context) ([]torrentstream.File, error)
	Select(int) (string, error)
	Progress() (completed, total int64)
	Close() error
}

type torrentPlayback struct {
	session torrentSession
	cancel  context.CancelFunc
	file    torrentstream.File
	path    string
	source  mediasource.Source
	once    sync.Once
	err     error
}

func (p *torrentPlayback) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		if p.session != nil {
			p.err = p.session.Close()
		}
	})
	return p.err
}

func (p *torrentPlayback) status() *domain.TorrentStatus {
	if p == nil || p.session == nil {
		return nil
	}
	completed, total := p.session.Progress()
	percent := float64(0)
	if total > 0 {
		percent = float64(completed) / float64(total) * 100
	}
	return &domain.TorrentStatus{
		FileIndex:       p.file.Index,
		FileName:        p.file.Name,
		DownloadedBytes: completed,
		TotalBytes:      total,
		ProgressPercent: percent,
	}
}

func isTorrentInput(source string) bool {
	source = strings.TrimSpace(source)
	return strings.HasPrefix(strings.ToLower(source), "magnet:") || strings.EqualFold(filepath.Ext(source), ".torrent")
}

func (m *Manager) validateTorrentInput(source string) (string, error) {
	source = strings.TrimSpace(source)
	if strings.HasPrefix(strings.ToLower(source), "magnet:") {
		return source, nil
	}
	if !strings.EqualFold(filepath.Ext(source), ".torrent") {
		return "", toolError("INVALID_ARGUMENT", "source must be a magnet URI or an absolute local .torrent path")
	}
	return m.validateLocalFilePath(source, "source")
}

// InspectTorrent requests metadata without selecting or downloading media.
func (m *Manager) InspectTorrent(ctx context.Context, req domain.InspectTorrentRequest) (*domain.InspectTorrentResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	input, err := m.validateTorrentInput(req.Source)
	if err != nil {
		return nil, err
	}
	timeoutMS := req.TimeoutMS
	if timeoutMS == 0 {
		timeoutMS = int(torrentMetadataTimeout / time.Millisecond)
	}
	if timeoutMS < torrentMinTimeoutMS || timeoutMS > torrentMaxTimeoutMS {
		return nil, toolError("INVALID_ARGUMENT", "timeout_ms must be between 1000 and 120000")
	}
	metadataCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	if m.torrentContext != nil {
		stopShutdown := context.AfterFunc(m.torrentContext, cancel)
		defer stopShutdown()
	}
	session, err := m.newTorrentSession(metadataCtx, input)
	if err != nil {
		if session != nil {
			_ = session.Close()
		}
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to open torrent", err)
	}
	if session == nil {
		return nil, toolError("TORRENT_ERROR", "torrent session is unavailable")
	}
	defer session.Close()
	files, err := session.Files(metadataCtx)
	if err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to read torrent metadata", err)
	}
	if err := metadataCtx.Err(); err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to read torrent metadata", err)
	}
	return &domain.InspectTorrentResult{OK: true, Files: torrentFiles(files)}, nil
}

// prepareTorrent keeps the torrent lifetime separate from the preparation
// request. The caller owns the returned playback resource after startup.
func (m *Manager) prepareTorrent(ctx context.Context, req domain.BeamRequest) (_ *torrentPlayback, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isClosed() {
		return nil, toolError("INTERNAL_ERROR", "beam manager is shutting down")
	}
	input, err := m.validateTorrentInput(req.Source)
	if err != nil {
		return nil, err
	}
	if req.EmbeddedSubtitleTrack != nil && strings.TrimSpace(req.SubtitlesPath) == "" && !strings.EqualFold(strings.TrimSpace(req.SubtitleMode), "off") {
		return nil, toolError("UNSUPPORTED_MEDIA", "embedded_subtitle_track is unavailable for torrent sources; omit it for automatic embedded text captions or provide subtitles_path")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetimeCtx := m.torrentContext
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	lifetimeCtx, cancel := context.WithCancel(lifetimeCtx)
	playback := &torrentPlayback{cancel: cancel}
	defer func() {
		if err != nil {
			_ = playback.Close()
		}
	}()
	metadataCtx, metadataCancel := context.WithTimeout(ctx, torrentMetadataTimeout)
	defer metadataCancel()
	stopPreparation := context.AfterFunc(metadataCtx, cancel)
	defer stopPreparation()
	playback.session, err = m.newTorrentSession(lifetimeCtx, input)
	if err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to open torrent", err)
	}
	if playback.session == nil {
		return nil, toolError("TORRENT_ERROR", "torrent session is unavailable")
	}
	files, err := playback.session.Files(metadataCtx)
	if err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to read torrent metadata", err)
	}
	if err := metadataCtx.Err(); err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to read torrent metadata", err)
	}
	if req.TorrentFileIndex == nil {
		if len(files) != 1 {
			if len(files) == 0 {
				return nil, toolError("TORRENT_ERROR", "torrent contains no supported media files")
			}
			return nil, &domain.ToolError{
				Code:           "TORRENT_FILE_REQUIRED",
				Message:        "torrent contains multiple media files; select torrent_file_index",
				SuggestedFixes: []string{"Use a file index from inspect_torrent."},
				Details:        map[string]any{"files": torrentFiles(files)},
			}
		}
		playback.file = files[0]
	} else {
		found := false
		for _, file := range files {
			if file.Index == *req.TorrentFileIndex {
				playback.file = file
				found = true
				break
			}
		}
		if !found {
			return nil, &domain.ToolError{
				Code:    "TORRENT_FILE_INVALID",
				Message: "torrent_file_index must identify a supported media file",
				Details: map[string]any{"files": torrentFiles(files)},
			}
		}
	}
	playback.path, err = playback.session.Select(playback.file.Index)
	if err != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "failed to select torrent media", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var registered bool
	playback.source, registered = mediasource.Lookup(playback.path)
	if !registered || playback.source == nil {
		return nil, toolError("TORRENT_ERROR", "selected torrent media source is unavailable")
	}
	// Disarm preparation cancellation before ownership passes to playback.
	// A watcher already running must win this handoff and release the source.
	if !stopPreparation() || metadataCtx.Err() != nil {
		return nil, torrentPreparationError(ctx, metadataCtx, "torrent preparation was canceled", context.Canceled)
	}
	return playback, nil
}

func (m *Manager) newTorrentSession(ctx context.Context, input string) (torrentSession, error) {
	if m.openTorrent != nil {
		session, err := m.openTorrent(ctx, input)
		// An interface wrapping Open's nil *Session is non-nil. Normalize it
		// before error cleanup so malformed inputs cannot panic in Close.
		if concrete, ok := session.(*torrentstream.Session); ok && concrete == nil {
			return nil, err
		}
		return session, err
	}
	session, err := torrentstream.Open(ctx, input)
	if session == nil {
		return nil, err
	}
	return session, err
}

func torrentFiles(files []torrentstream.File) []domain.TorrentFile {
	result := make([]domain.TorrentFile, 0, len(files))
	for _, file := range files {
		result = append(result, domain.TorrentFile{Index: file.Index, Name: file.Name, Size: file.Size})
	}
	return result
}

func torrentPreparationError(requestCtx, metadataCtx context.Context, message string, err error) error {
	if requestCtx.Err() != nil {
		return requestCtx.Err()
	}
	if errors.Is(metadataCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return toolError("TORRENT_METADATA_TIMEOUT", "timed out waiting for torrent metadata; retry when peers are available")
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return toolError("TORRENT_ERROR", fmt.Sprintf("%s: %v", message, err))
}

type combinedCloser struct {
	closers []io.Closer
	once    sync.Once
	err     error
}

func combineClosers(closers ...io.Closer) io.Closer {
	combined := &combinedCloser{}
	for _, closer := range closers {
		if closer != nil {
			combined.closers = append(combined.closers, closer)
		}
	}
	if len(combined.closers) == 0 {
		return nil
	}
	return combined
}

func (c *combinedCloser) Close() error {
	c.once.Do(func() {
		for _, closer := range c.closers {
			c.err = errors.Join(c.err, closer.Close())
		}
	})
	return c.err
}
