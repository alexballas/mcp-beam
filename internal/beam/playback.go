package beam

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go2tv.app/go2tv/v2/castsubtitles"
	"go2tv.app/go2tv/v2/mediasource"
	"go2tv.app/go2tv/v2/utils"
	"go2tv.app/mcp-beam/internal/domain"
)

func (m *Manager) preparePlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string, torrent *torrentPlayback) (*preparedPlayback, error) {
	if strings.TrimSpace(req.Source) == "" {
		return nil, toolError("UNSUPPORTED_MEDIA", "source is empty")
	}
	if torrent != nil {
		return m.prepareFilePlayback(ctx, req, device, mode, torrent)
	}
	if parsed, err := url.Parse(strings.TrimSpace(req.Source)); err == nil && parsed.Scheme != "" {
		return m.prepareURLPlayback(ctx, req, device, mode)
	}
	return m.prepareFilePlayback(ctx, req, device, mode, nil)
}

func (m *Manager) prepareFilePlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string, torrent *torrentPlayback) (_ *preparedPlayback, err error) {
	source := strings.TrimSpace(req.Source)
	mediaType := ""
	if torrent != nil {
		source, mediaType = torrent.path, torrent.source.MIME()
	} else {
		source, err = m.validateLocalFilePath(source, "source")
		if err != nil {
			return nil, err
		}
		mediaType = detectFileMediaType(source)
	}
	subtitleMode := normalizedPlaybackSubtitleMode(req.SubtitleMode)
	subtitlesPath := ""
	if subtitleMode != "off" {
		if torrent == nil && req.EmbeddedSubtitleTrack == nil {
			subtitlesPath, err = m.resolveSubtitlePathForFile(source, req.SubtitlesPath)
		} else if strings.TrimSpace(req.SubtitlesPath) != "" {
			subtitlesPath, err = m.validateLocalFilePath(req.SubtitlesPath, "subtitles_path")
		}
		if err != nil {
			return nil, err
		}
	}
	transcoding, warnings, ffmpegPath, err := m.computeTranscodingForFile(ctx, mode, mediaType, source)
	if err != nil {
		return nil, err
	}
	embeddedTrack := req.EmbeddedSubtitleTrack
	if subtitlesPath != "" || subtitleMode == "off" || torrent != nil {
		embeddedTrack = nil
	}
	if subtitleMode != "off" {
		ffmpegPath, err = m.ensureSubtitleFFmpeg(subtitlesPath, embeddedTrack, ffmpegPath)
		if err != nil {
			return nil, err
		}
	}
	listenAddr, server, err := m.newStreamServer(device.Address)
	if err != nil {
		return nil, err
	}
	prepared := &preparedPlayback{httpServer: server, warnings: warnings}
	defer func() {
		if err != nil {
			cleanupPrepared(prepared)
		}
	}()
	startAt := startSeconds(req.StartSeconds)
	selected, err := m.prepareLocalSubtitles(ctx, server, listenAddr, source, subtitlesPath, ffmpegPath, transcoding, subtitleMode, embeddedTrack, startAt)
	prepared.sourceCloser = selected.closer
	if err != nil {
		return nil, err
	}
	prepared.warnings = append(prepared.warnings, selected.warnings...)
	prepared.subtitleURL = selected.subtitleURL
	var torrentCaptionSource mediasource.Source
	torrentSubtitleRoute := ""
	if torrent != nil && subtitlesPath == "" && subtitleMode != "off" {
		torrentCaptionSource = castsubtitles.TorrentSource(source, true)
		if torrentCaptionSource != nil && (!transcoding || subtitleMode == "receiver") {
			torrentSubtitleRoute = "/torrent-subs-" + randomToken(8) + ".json"
			offset := 0
			if transcoding {
				offset = startAt
			}
			server.AddHandler(torrentSubtitleRoute, nil, nil, castsubtitles.TorrentHandler(torrentCaptionSource, offset, transcoding))
			prepared.torrentSubtitleURL = "http://" + listenAddr + torrentSubtitleRoute
		}
	}
	route := mediaRouteFor(source)
	var tcOpts *utils.TranscodeOptions
	if transcoding {
		tcOpts = &utils.TranscodeOptions{
			FFmpegPath:       ffmpegPath,
			SubsPath:         selected.burnPath,
			EmbeddedSubtitle: selected.embedded,
			SeekSeconds:      startAt,
			SubtitleSize:     utils.SubtitleSizeMedium,
		}
		if subtitleMode == "auto" || subtitleMode == "burn" {
			tcOpts.TorrentSource = torrentCaptionSource
		}
		prepared.castSeekPlan = &chromecastTranscodeSeek{
			sourcePath: source, ffmpegPath: ffmpegPath, subsPath: subtitlesPath, route: route,
			subtitleMode: subtitleMode, embeddedTrack: embeddedTrack,
			torrentSource: torrentCaptionSource, torrentSubtitleRoute: torrentSubtitleRoute,
		}
		mediaType = "video/mp4"
		duration, durationErr := m.optionalMediaDuration(ctx, ffmpegPath, source)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if durationErr == nil {
			prepared.mediaDuration = duration
		} else {
			prepared.warnings = append(prepared.warnings, "failed to determine media duration for transcoded stream")
		}
	}
	server.AddHandler(route, nil, tcOpts, source)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := startStreamServer(server); err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start media server: %v", err))
	}
	prepared.mediaURL = "http://" + listenAddr + route
	prepared.mediaType = mediaType
	prepared.title = mediaTitleFor(source)
	prepared.transcoding = transcoding
	return prepared, nil
}

func (m *Manager) prepareURLPlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (_ *preparedPlayback, err error) {
	sourceURL := strings.TrimSpace(req.Source)
	if _, err := m.validateSourceURLPolicy(sourceURL); err != nil {
		return nil, err
	}
	if normalizedPlaybackSubtitleMode(req.SubtitleMode) != "off" && strings.TrimSpace(req.SubtitlesPath) != "" {
		req.SubtitlesPath, err = m.validateLocalFilePath(req.SubtitlesPath, "subtitles_path")
		if err != nil {
			return nil, err
		}
	}
	if utils.IsHLSStream(sourceURL, "") {
		if mode == transcodeAlways {
			return nil, toolError("TRANSCODE_REQUIRED", "transcode=always is not supported for direct HLS URL casting")
		}
		return m.prepareDirectURLPlayback(ctx, req, device, "application/vnd.apple.mpegurl", []string{})
	}
	_, mediaType, mediaCloser, err := m.prepareRetainedURLMedia(ctx, sourceURL)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to stream source URL: %v", err))
	}
	// Preparation only probes the type. Renderer GETs reopen the original URL.
	if mediaCloser != nil {
		_ = mediaCloser.Close()
	}
	if mediaType == "" || mediaType == "/" {
		mediaType = "application/octet-stream"
	}
	warnings := []string{}
	transcoding := mode == transcodeAlways && strings.Contains(mediaType, "video")
	if !transcoding {
		if mode == transcodeAuto && strings.Contains(mediaType, "video") {
			warnings = append(warnings, "auto transcode for URL sources defaults to direct URL stream")
		}
		if mode == transcodeAlways {
			warnings = append(warnings, "transcode=always ignored for non-video URL source")
		}
		return m.prepareDirectURLPlayback(ctx, req, device, mediaType, warnings)
	}
	prepared := &preparedPlayback{warnings: warnings}
	defer func() {
		if err != nil {
			cleanupPrepared(prepared)
		}
	}()
	ffmpegPath, err := m.requireFFmpeg()
	if err != nil {
		return nil, err
	}
	if normalizedPlaybackSubtitleMode(req.SubtitleMode) != "off" {
		embeddedTrack := req.EmbeddedSubtitleTrack
		if strings.TrimSpace(req.SubtitlesPath) != "" {
			embeddedTrack = nil
		}
		ffmpegPath, err = m.ensureSubtitleFFmpeg(req.SubtitlesPath, embeddedTrack, ffmpegPath)
		if err != nil {
			return nil, err
		}
	}
	listenAddr, server, err := m.newStreamServer(device.Address)
	if err != nil {
		return nil, err
	}
	prepared.httpServer = server
	startAt := startSeconds(req.StartSeconds)
	selected, err := m.prepareLocalSubtitles(ctx, server, listenAddr, "", req.SubtitlesPath, ffmpegPath, true, req.SubtitleMode, req.EmbeddedSubtitleTrack, startAt)
	prepared.sourceCloser = selected.closer
	if err != nil {
		return nil, err
	}
	prepared.warnings = append(prepared.warnings, selected.warnings...)
	route := mediaRouteFor(sourceURL)
	tcOpts := &utils.TranscodeOptions{
		FFmpegPath: ffmpegPath, SubsPath: selected.burnPath, EmbeddedSubtitle: selected.embedded,
		SeekSeconds: startAt, SubtitleSize: utils.SubtitleSizeMedium,
	}
	server.AddHandler(route, nil, nil, m.chromecastURLHandler(sourceURL, tcOpts))
	prepared.castSeekPlan = &chromecastTranscodeSeek{
		sourcePath: sourceURL, urlSource: true, ffmpegPath: ffmpegPath,
		subsPath: req.SubtitlesPath, route: route, subtitleMode: req.SubtitleMode,
	}
	prepared.mediaDuration, err = optionalURLMediaDuration(ctx, ffmpegPath, sourceURL)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := startStreamServer(server); err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start media server: %v", err))
	}
	prepared.mediaURL = "http://" + listenAddr + route
	prepared.mediaType = "video/mp4"
	prepared.title = mediaTitleFor(sourceURL)
	prepared.subtitleURL = selected.subtitleURL
	prepared.live = prepared.mediaDuration <= 0
	prepared.transcoding = true
	return prepared, nil
}

func (m *Manager) prepareDirectURLPlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mediaType string, warnings []string) (_ *preparedPlayback, err error) {
	sourceURL := strings.TrimSpace(req.Source)
	prepared := &preparedPlayback{
		mediaURL: sourceURL, mediaType: mediaType, title: mediaTitleFor(sourceURL),
		live: true, warnings: append([]string{}, warnings...),
	}
	subtitleMode := normalizedPlaybackSubtitleMode(req.SubtitleMode)
	if subtitleMode == "off" {
		return prepared, nil
	}
	if subtitleMode == "burn" {
		return nil, toolError("TRANSCODE_REQUIRED", "burning subtitles requires transcoding; set transcode to always")
	}
	if subtitleMode != "auto" && subtitleMode != "receiver" {
		return nil, toolError("INVALID_ARGUMENT", "subtitle_mode must be auto, receiver, burn, or off")
	}
	if strings.TrimSpace(req.SubtitlesPath) == "" {
		if req.EmbeddedSubtitleTrack != nil {
			return nil, toolError("UNSUPPORTED_MEDIA", "embedded subtitles require a local media file")
		}
		return prepared, nil
	}
	ffmpegPath, err := m.ensureSubtitleFFmpeg(req.SubtitlesPath, nil, "")
	if err != nil {
		return nil, err
	}
	listenAddr, server, err := m.newStreamServer(device.Address)
	if err != nil {
		return nil, err
	}
	prepared.httpServer = server
	defer func() {
		if err != nil {
			cleanupPrepared(prepared)
		}
	}()
	selected, err := m.prepareLocalSubtitles(ctx, server, listenAddr, "", req.SubtitlesPath, ffmpegPath, false, subtitleMode, req.EmbeddedSubtitleTrack, 0)
	prepared.sourceCloser = selected.closer
	if err != nil {
		return nil, err
	}
	prepared.warnings = append(prepared.warnings, selected.warnings...)
	if selected.subtitleURL == "" {
		cleanupPrepared(prepared)
		prepared.httpServer, prepared.sourceCloser = nil, nil
		return prepared, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := startStreamServer(server); err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start subtitle server: %v", err))
	}
	prepared.subtitleURL = selected.subtitleURL
	return prepared, nil
}

func normalizedPlaybackSubtitleMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return "auto"
	}
	return mode
}
