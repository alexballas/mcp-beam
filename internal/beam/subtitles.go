package beam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go2tv.app/go2tv/v2/utils"
)

// localSubtitles holds the captions selected for a prepared local playback.
// A closer must remain alive with the session when burnPath is a temporary file.
type localSubtitles struct {
	subtitleURL string
	burnPath    string
	embedded    *utils.EmbeddedSubtitle
	warnings    []string
	closer      io.Closer
	sidecar     any
}

func (m *Manager) prepareLocalSubtitles(
	ctx context.Context,
	server streamServer,
	listenAddr, mediaPath, subtitlePath, ffmpegPath string,
	transcoding bool,
	mode string,
	embeddedTrack *int,
	seekSeconds int,
) (localSubtitles, error) {
	selected, sourcePath, err := m.selectLocalSubtitles(ctx, mediaPath, subtitlePath, ffmpegPath, transcoding, mode, embeddedTrack)
	if err != nil || sourcePath == "" {
		return selected, err
	}
	// Native receiver playback keeps the original timeline. A transcoded
	// stream starts at zero, so its receiver cues use the requested seek offset.
	offset := 0
	if transcoding {
		offset = seekSeconds
	}
	data, err := utils.SubtitlesForPlaybackContext(ctx, sourcePath, offset, ffmpegPath)
	selected.closeTemporarySource()
	if err != nil {
		prepareErr := localSubtitlePreparationError(ctx, mode, &selected, "failed to convert subtitles to WebVTT", err)
		return selected, prepareErr
	}
	if server == nil {
		return selected, toolError("INTERNAL_ERROR", "subtitle stream server is not configured")
	}
	route := "/subs-" + randomToken(8) + ".vtt"
	server.AddHandler(route, nil, nil, data)
	selected.subtitleURL = "http://" + listenAddr + route
	return selected, nil
}

// prepareDLNALocalSubtitles returns an SRT sidecar for receiver playback.
// Its burn selection is shared with Chromecast so styled and bitmap tracks
// retain their source container and its timeline origin.
func (m *Manager) prepareDLNALocalSubtitles(
	ctx context.Context,
	mediaPath, subtitlePath, ffmpegPath string,
	transcoding bool,
	mode string,
	embeddedTrack *int,
	seekSeconds int,
) (localSubtitles, error) {
	selected, sourcePath, err := m.selectLocalSubtitles(ctx, mediaPath, subtitlePath, ffmpegPath, transcoding, mode, embeddedTrack)
	if err != nil || sourcePath == "" {
		return selected, err
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		selected.closeTemporarySource()
		prepareErr := localSubtitlePreparationError(ctx, mode, &selected, "failed to read DLNA subtitles", err)
		return selected, prepareErr
	}
	// Transcoded DLNA playback also starts at zero. Shift through WebVTT
	// before preparing SRT so caption timing follows the actual media stream.
	var data []byte
	if transcoding && seekSeconds > 0 {
		data, err = utils.SubtitlesForPlaybackContext(ctx, sourcePath, seekSeconds, ffmpegPath)
		if err == nil {
			data, err = utils.SubtitlesReaderToSRT(ctx, strings.NewReader(string(data)), ".vtt", ffmpegPath)
		}
	} else {
		data, err = utils.SubtitlesReaderToSRT(ctx, file, filepath.Ext(sourcePath), ffmpegPath)
	}
	err = errors.Join(err, file.Close())
	selected.closeTemporarySource()
	if err != nil {
		prepareErr := localSubtitlePreparationError(ctx, mode, &selected, "failed to convert DLNA subtitles to SRT", err)
		return selected, prepareErr
	}
	selected.sidecar = data
	return selected, nil
}

// selectLocalSubtitles leaves a receiver source to be converted by the caller.
// External captions take precedence over any embedded track selection.
func (m *Manager) selectLocalSubtitles(
	ctx context.Context,
	mediaPath, subtitlePath, ffmpegPath string,
	transcoding bool,
	mode string,
	embeddedTrack *int,
) (localSubtitles, string, error) {
	selected := localSubtitles{}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "off":
		return selected, "", nil
	case "auto", "receiver", "burn":
	default:
		return selected, "", toolError("INVALID_ARGUMENT", "subtitle_mode must be auto, receiver, burn, or off")
	}
	if err := ctx.Err(); err != nil {
		return selected, "", err
	}
	subtitlePath = strings.TrimSpace(subtitlePath)
	burn := mode == "burn" || (mode == "auto" && transcoding)
	if burn && !transcoding {
		return selected, "", toolError("TRANSCODE_REQUIRED", "burning subtitles requires transcoding; set transcode to always")
	}
	if subtitlePath == "" && embeddedTrack == nil {
		return selected, "", nil
	}
	if subtitlePath != "" {
		validated, err := m.validateLocalFilePath(subtitlePath, "subtitles_path")
		if err != nil {
			return selected, "", err
		}
		switch strings.ToLower(filepath.Ext(validated)) {
		case ".srt", ".vtt", ".ass", ".ssa":
		default:
			message := "unsupported subtitle format; expected .srt, .vtt, .ass, or .ssa"
			if mode == "auto" {
				selected.warnings = append(selected.warnings, message)
				return selected, "", nil
			}
			return selected, "", toolError("UNSUPPORTED_MEDIA", message)
		}
		if burn {
			selected.burnPath = validated
			return selected, "", nil
		}
		if extension := strings.ToLower(filepath.Ext(validated)); extension == ".ass" || extension == ".ssa" {
			selected.warnings = append(selected.warnings, "receiver subtitles do not preserve ASS/SSA styling")
		}
		return selected, validated, nil
	}
	if *embeddedTrack < 0 {
		return selected, "", toolError("INVALID_ARGUMENT", "embedded_subtitle_track must be a non-negative subtitle stream index")
	}
	if strings.TrimSpace(mediaPath) == "" {
		return selected, "", toolError("UNSUPPORTED_MEDIA", "embedded subtitles require a local media file")
	}
	embedded, err := utils.EmbeddedSubtitleForBurnContext(ctx, ffmpegPath, mediaPath, *embeddedTrack)
	if err != nil {
		return selected, "", localSubtitlePreparationError(ctx, "receiver", &selected, "failed to inspect embedded subtitles", err)
	}
	if embedded != nil && embedded.Bitmap && !burn {
		return selected, "", toolError("TRANSCODE_REQUIRED", "bitmap subtitles require burning into transcoded video; set subtitle_mode to burn and transcode to always")
	}
	if embedded != nil && burn {
		// Direct text burn keeps the selected container's original timeline.
		selected.embedded = embedded
		return selected, "", nil
	}
	extracted, err := utils.ExtractSubContext(ctx, ffmpegPath, *embeddedTrack, mediaPath)
	if err != nil {
		if errors.Is(err, utils.ErrBitmapSubtitles) {
			return selected, "", toolError("TRANSCODE_REQUIRED", "bitmap subtitles require burning into transcoded video; set subtitle_mode to burn and transcode to always")
		}
		return selected, "", localSubtitlePreparationError(ctx, "receiver", &selected, "failed to extract embedded subtitles", err)
	}
	selected.closer = &localSubtitleFileCloser{path: extracted}
	if burn {
		selected.burnPath = extracted
		return selected, "", nil
	}
	if embedded != nil && !embedded.PlainText {
		selected.warnings = append(selected.warnings, "receiver subtitles do not preserve embedded ASS/SSA styling")
	}
	return selected, extracted, nil
}

func localSubtitlePreparationError(ctx context.Context, mode string, selected *localSubtitles, message string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if strings.TrimSpace(mode) == "" || strings.EqualFold(strings.TrimSpace(mode), "auto") {
		selected.warnings = append(selected.warnings, message)
		return nil
	}
	return toolError("PROTOCOL_ERROR", fmt.Sprintf("%s: %v", message, err))
}

func (s *localSubtitles) closeTemporarySource() {
	if s.closer != nil {
		_ = s.closer.Close()
		s.closer = nil
	}
}

type localSubtitleFileCloser struct {
	path string
	once sync.Once
	err  error
}

func (c *localSubtitleFileCloser) Close() error {
	c.once.Do(func() {
		c.err = os.Remove(c.path)
		if errors.Is(c.err, os.ErrNotExist) {
			c.err = nil
		}
	})
	return c.err
}
