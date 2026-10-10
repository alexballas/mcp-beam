package beam

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"go2tv.app/go2tv/v2/castsubtitles"
	"go2tv.app/go2tv/v2/mediasource"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/go2tv/v2/utils"
	"go2tv.app/mcp-beam/internal/adapters"
	"go2tv.app/mcp-beam/internal/domain"
)

type dlnaSubtitleSeek struct {
	mediaPath     string
	subtitlePath  string
	ffmpegPath    string
	mode          string
	embeddedTrack *int
}

type dlnaPlaybackOptions struct {
	captions      localSubtitles
	torrentSource mediasource.Source
}

func (m *Manager) ensureSubtitleFFmpeg(subtitlePath string, embeddedTrack *int, ffmpegPath string) (string, error) {
	if ffmpegPath != "" {
		return ffmpegPath, nil
	}
	ext := strings.ToLower(filepath.Ext(subtitlePath))
	if embeddedTrack != nil || ext == ".ass" || ext == ".ssa" {
		return m.requireFFmpeg()
	}
	return "", nil
}

func (m *Manager) prepareDLNAFilePlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string, torrent *torrentPlayback) (*preparedDLNA, error) {
	source := req.Source
	mediaType := ""
	var err error
	if torrent != nil {
		mediaType = torrent.source.MIME()
	} else {
		source, err = m.validateLocalFilePath(source, "source")
		if err != nil {
			return nil, err
		}
		mediaType = detectFileMediaType(source)
	}
	subtitlesPath := ""
	track := req.EmbeddedSubtitleTrack
	if req.SubtitleMode != "off" {
		if torrent == nil && track == nil {
			subtitlesPath, err = m.resolveSubtitlePathForFile(source, req.SubtitlesPath)
		} else {
			subtitlesPath, err = m.validateLocalFilePath(req.SubtitlesPath, "subtitles_path")
		}
		if err != nil {
			return nil, err
		}
		if subtitlesPath != "" {
			track = nil
		}
	} else {
		track = nil
	}
	transcoding, warnings, ffmpegPath, err := m.computeTranscodingForDLNA(mode, mediaType)
	if err != nil {
		return nil, err
	}
	ffmpegPath, err = m.ensureSubtitleFFmpeg(subtitlesPath, track, ffmpegPath)
	if err != nil {
		return nil, err
	}
	captions, err := m.prepareDLNALocalSubtitles(ctx, source, subtitlesPath, ffmpegPath, transcoding, req.SubtitleMode, track, startSeconds(req.StartSeconds))
	if err != nil {
		captions.closeTemporarySource()
		return nil, err
	}
	options := dlnaPlaybackOptions{captions: captions}
	duration := float64(0)
	if transcoding {
		duration, err = m.optionalMediaDuration(ctx, ffmpegPath, source)
		if err != nil {
			captions.closeTemporarySource()
			return nil, err
		}
	}
	if torrent != nil && subtitlesPath == "" && req.SubtitleMode != "off" {
		if transcoding && req.SubtitleMode != "receiver" {
			options.torrentSource = castsubtitles.TorrentSource(source, true)
		} else if castsubtitles.TorrentSource(source, true) != nil {
			warnings = append(warnings, "DLNA progressive embedded captions require subtitle_mode=burn and transcoding, or an external subtitles_path")
		}
	}
	prepared, err := m.startDLNAServerAndPlay(ctx, device, source, mediaType, source, transcoding, ffmpegPath, startSeconds(req.StartSeconds), options)
	if err != nil {
		captions.closeTemporarySource()
		return nil, err
	}
	prepared.title = mediaTitleFor(source)
	prepared.contentType = mediaType
	prepared.mediaDuration = duration
	prepared.warnings = append(prepared.warnings, warnings...)
	if transcoding {
		prepared.dlnaSeekPlan = &dlnaSubtitleSeek{mediaPath: source, subtitlePath: subtitlesPath, ffmpegPath: ffmpegPath, mode: req.SubtitleMode, embeddedTrack: track}
	}
	return prepared, nil
}

func (m *Manager) prepareDLNAURLPlayback(ctx context.Context, req domain.BeamRequest, device *domain.Device, mode string) (*preparedDLNA, error) {
	sourceURL := strings.TrimSpace(req.Source)
	if _, err := m.validateSourceURLPolicy(sourceURL); err != nil {
		return nil, err
	}
	if utils.IsHLSStream(sourceURL, "") {
		return nil, dlnaHLSUnsupportedError()
	}
	subtitlesPath := ""
	var err error
	if req.SubtitleMode != "off" {
		subtitlesPath, err = m.validateDLNASubtitles(req.SubtitlesPath)
		if err != nil {
			return nil, err
		}
	}
	_, mediaType, mediaCloser, err := m.prepareRetainedURLMedia(ctx, sourceURL)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to stream source URL: %v", err))
	}
	// Probe bodies never belong to the session: its handler opens each request.
	if mediaCloser != nil {
		_ = mediaCloser.Close()
	}
	if mediaType == "" || mediaType == "/" {
		mediaType = detectURLMediaType(sourceURL)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
	}
	if utils.IsHLSStream(sourceURL, mediaType) {
		return nil, dlnaHLSUnsupportedError()
	}
	transcoding, warnings, ffmpegPath, err := m.computeTranscodingForDLNA(mode, mediaType)
	if err != nil {
		return nil, err
	}
	ffmpegPath, err = m.ensureSubtitleFFmpeg(subtitlesPath, nil, ffmpegPath)
	if err != nil {
		return nil, err
	}
	captions, err := m.prepareDLNALocalSubtitles(ctx, "", subtitlesPath, ffmpegPath, transcoding, req.SubtitleMode, nil, startSeconds(req.StartSeconds))
	if err != nil {
		captions.closeTemporarySource()
		return nil, err
	}
	duration := float64(0)
	if transcoding {
		duration, err = optionalURLMediaDuration(ctx, ffmpegPath, sourceURL)
		if err != nil {
			captions.closeTemporarySource()
			return nil, err
		}
	}
	prepared, err := m.startDLNAServerAndPlay(ctx, device, sourceURL, mediaType, sourceURL, transcoding, ffmpegPath, startSeconds(req.StartSeconds), dlnaPlaybackOptions{captions: captions})
	if err != nil {
		captions.closeTemporarySource()
		return nil, err
	}
	prepared.title = mediaTitleFor(sourceURL)
	prepared.contentType = mediaType
	prepared.warnings = append(prepared.warnings, warnings...)
	prepared.mediaDuration = duration
	if transcoding {
		prepared.dlnaSeekPlan = &dlnaSubtitleSeek{mediaPath: sourceURL, subtitlePath: subtitlesPath, ffmpegPath: ffmpegPath, mode: req.SubtitleMode}
	}
	return prepared, nil
}

func (m *Manager) startDLNAServerAndPlay(ctx context.Context, device *domain.Device, media any, mediaType, mediaPath string, transcoding bool, ffmpegPath string, startAtSeconds int, options dlnaPlaybackOptions) (*preparedDLNA, error) {
	startupCtx, cancel := m.operationContext(ctx)
	defer cancel()
	payload, err := m.newDLNAPayload(startupCtx, device, mediaPath, mediaType, transcoding, ffmpegPath, startAtSeconds, options)
	if err != nil {
		return nil, err
	}
	server := m.serverFactory.New(payload.ListenAddress())
	raw := payload.RawPayload()
	if raw == nil {
		return nil, toolError("INTERNAL_ERROR", "DLNA payload details are unavailable")
	}
	var proxyCloser io.Closer
	transferred := false
	defer func() {
		if !transferred && proxyCloser != nil {
			_ = proxyCloser.Close()
		}
	}()
	if sourceURL, ok := media.(string); ok {
		if parsed, parseErr := url.Parse(sourceURL); parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			if transcoding {
				media = dlnaURLTranscodeHandler(sourceURL, raw)
			} else {
				media, proxyCloser, err = nativeURLProxy(sourceURL, mediaType)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	// StartServer only registers direct-play subtitles. Receiver captions on a
	// transcoded stream need their own handler as well as the advertised URL.
	if options.captions.sidecar != nil {
		route := "/subs-" + randomToken(8) + ".srt"
		raw.SubtitlesURL = "http://" + payload.ListenAddress() + route
		if transcoding {
			server.AddHandler(route, nil, nil, options.captions.sidecar)
		}
	} else {
		raw.SubtitlesURL = ""
	}
	serverStarted := make(chan error, 1)
	callbackStateCh := make(chan string, dlnaCallbackQueueSize)
	screen := &dlnaMonitorScreen{stateCh: callbackStateCh}
	go server.StartServer(serverStarted, media, options.captions.sidecar, raw, screen)
	// StartServer signals once after binding; drain that signal before stopping
	// it so cancellation cannot leave a listener opening after cleanup.
	if err := <-serverStarted; err != nil {
		server.StopServer()
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start DLNA media server: %v", err))
	}
	if err := m.withRetryAwait(startupCtx, func() error { return payload.SendtoTVContext(startupCtx, "Play1") }); err != nil {
		server.StopServer()
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to start DLNA playback: %v", err))
	}
	if !transcoding && startAtSeconds > 0 {
		reltime := utils.SecondsToClockTime(startAtSeconds)
		if err := m.withRetryAwait(startupCtx, func() error { return payload.SeekSoapCall(reltime) }); err != nil {
			server.StopServer()
			_ = payload.SendtoTV("Stop")
			return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to seek DLNA playback start position: %v", err))
		}
	}
	transferred = true
	return &preparedDLNA{mediaURL: payload.MediaURL(), transcoding: transcoding, warnings: options.captions.warnings, httpServer: server, payload: payload, callbackCh: callbackStateCh, sourceCloser: combineClosers(options.captions.closer, proxyCloser)}, nil
}

func (m *Manager) newDLNAPayload(ctx context.Context, device *domain.Device, mediaPath, mediaType string, transcoding bool, ffmpegPath string, startAtSeconds int, options dlnaPlaybackOptions) (adapters.DLNAPayload, error) {
	if strings.TrimSpace(mediaPath) == "" {
		return nil, toolError("UNSUPPORTED_MEDIA", "source is empty")
	}
	payload, err := m.dlnaFactory.NewTVPayload(&soapcalls.Options{
		Ctx: ctx, DMR: device.Address, Media: mediaPath, Mtype: mediaType,
		Transcode: transcoding, Seek: !transcoding, FFmpegPath: ffmpegPath,
		FFmpegSubsPath: options.captions.burnPath, FFmpegEmbeddedSubtitle: options.captions.embedded,
		FFmpegSeek: startAtSeconds, TorrentSource: options.torrentSource,
	})
	if err != nil {
		return nil, toolError("PROTOCOL_ERROR", fmt.Sprintf("failed to initialize DLNA payload: %v", err))
	}
	payload.SetContext(ctx)
	return payload, nil
}

func (m *Manager) refreshDLNASeekSubtitles(ctx context.Context, sess *session, offset int) error {
	plan := sess.dlnaSeekPlan
	if plan == nil {
		raw := sess.dlnaPayload.RawPayload()
		options := raw.TranscodeOptionsSnapshot()
		if sess.httpServer != nil {
			sess.httpServer.SetDLNATranscodeSeek(raw, offset, options.SubsPath, options.EmbeddedSubtitle, "", nil)
		} else {
			raw.SetTranscodeSeek(offset, options.SubsPath, options.EmbeddedSubtitle)
		}
		return nil
	}
	captions, err := m.prepareDLNALocalSubtitles(ctx, plan.mediaPath, plan.subtitlePath, plan.ffmpegPath, true, plan.mode, plan.embeddedTrack, offset)
	if err != nil {
		captions.closeTemporarySource()
		return err
	}
	if err := ctx.Err(); err != nil {
		captions.closeTemporarySource()
		return err
	}
	raw := sess.dlnaPayload.RawPayload()
	subtitleRoute := ""
	if captions.sidecar != nil {
		parsed, err := url.Parse(raw.SubtitlesURL)
		if err != nil || parsed.Host == "" || sess.httpServer == nil {
			captions.closeTemporarySource()
			return fmt.Errorf("subtitle stream server is unavailable")
		}
		subtitleRoute = parsed.Path
	}
	if sess.httpServer != nil {
		sess.httpServer.SetDLNATranscodeSeek(raw, offset, captions.burnPath, captions.embedded, subtitleRoute, captions.sidecar)
	} else {
		raw.SetTranscodeSeek(offset, captions.burnPath, captions.embedded)
	}
	sess.sourceCloser = combineClosers(sess.sourceCloser, captions.closer)
	return nil
}

// Use a small optional probe budget; metadata that has not arrived should not
// consume the entire torrent preparation timeout just to estimate duration.
func (m *Manager) optionalMediaDuration(ctx context.Context, ffmpegPath, source string) (float64, error) {
	if ffmpegPath == "" {
		return 0, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	duration, err := utils.DurationForMediaSecondsContext(probeCtx, ffmpegPath, source)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, nil
	}
	return duration, nil
}
