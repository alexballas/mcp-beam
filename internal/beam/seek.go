package beam

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/castsubtitles"
	"go2tv.app/go2tv/v2/metadata"
	"go2tv.app/go2tv/v2/utils"
)

func (m *Manager) seekChromecastTranscoded(ctx context.Context, sess *session, resolvedSeconds int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		return errors.New("chromecast session is not configured")
	}
	sess.playbackMu.Lock()
	defer sess.playbackMu.Unlock()
	ctx, cancelSeek := context.WithCancel(ctx)
	sess.seekCancelMu.Lock()
	if sess.seekClosing {
		sess.seekCancelMu.Unlock()
		cancelSeek()
		return errors.New("chromecast session is closing")
	}
	sess.seekCancel = cancelSeek
	sess.seekCancelMu.Unlock()
	defer func() {
		sess.seekCancelMu.Lock()
		sess.seekCancel = nil
		sess.seekCancelMu.Unlock()
		cancelSeek()
	}()
	if sess.closed {
		return errors.New("chromecast session is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess.castClient == nil {
		return errors.New("chromecast session is not configured")
	}
	if sess.castSeekPlan == nil {
		return errors.New("transcoded seek is unsupported for this chromecast session")
	}
	if sess.httpServer == nil {
		return errors.New("stream server is not configured")
	}
	currentURL := sess.streamURL()
	parsedMediaURL, err := url.Parse(currentURL)
	if err != nil || parsedMediaURL.Host == "" {
		return fmt.Errorf("invalid session media url: %s", currentURL)
	}
	plan := sess.castSeekPlan
	if plan.torrentSubtitleRoute != "" && plan.torrentSource == nil {
		return errors.New("torrent subtitle source is not configured")
	}
	mediaPath := plan.sourcePath
	if plan.urlSource {
		mediaPath = ""
	}
	captions, err := m.prepareLocalSubtitles(ctx, sess.httpServer, parsedMediaURL.Host, mediaPath, plan.subsPath, plan.ffmpegPath, true, plan.subtitleMode, plan.embeddedTrack, resolvedSeconds)
	if err != nil {
		captions.closeTemporarySource()
		return err
	}
	// Until the transcode handler is installed, a temporary burn file belongs
	// to this preparation and can be removed if cancellation interrupts it.
	defer captions.closeTemporarySource()
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := &utils.TranscodeOptions{
		FFmpegPath: plan.ffmpegPath, SubsPath: captions.burnPath,
		EmbeddedSubtitle: captions.embedded, SeekSeconds: resolvedSeconds,
		SubtitleSize: utils.SubtitleSizeMedium,
	}
	mode := strings.ToLower(strings.TrimSpace(plan.subtitleMode))
	if mode == "" {
		mode = "auto"
	}
	if plan.torrentSource != nil && plan.torrentSubtitleRoute == "" && captions.burnPath == "" && captions.embedded == nil && (mode == "auto" || mode == "burn") {
		opts.TorrentSource = plan.torrentSource
	}
	torrentSubtitleURL := ""
	if plan.torrentSubtitleRoute != "" {
		sess.httpServer.AddHandler(plan.torrentSubtitleRoute, nil, nil, castsubtitles.TorrentHandler(plan.torrentSource, resolvedSeconds, true))
		torrentSubtitleURL = "http://" + parsedMediaURL.Host + plan.torrentSubtitleRoute
	}
	if plan.urlSource {
		sess.httpServer.AddHandler(plan.route, nil, nil, m.chromecastURLHandler(plan.sourcePath, opts))
	} else {
		sess.httpServer.AddHandler(plan.route, nil, opts, plan.sourcePath)
	}
	// The published handler may be fetched even if the subsequent reload
	// fails, so retain its subtitle file until the session is shut down.
	if captions.closer != nil {
		sess.sourceCloser = combineClosers(sess.sourceCloser, captions.closer)
		captions.closer = nil
	}
	mediaURL := "http://" + parsedMediaURL.Host + plan.route
	request := castprotocol.LoadRequest{
		MediaURL: mediaURL, ContentType: "video/mp4",
		Metadata: metadata.Media{Title: sess.Title}, StartTime: 0,
		Duration:    remainingDuration(sess.mediaDuration, resolvedSeconds, true),
		SubtitleURL: captions.subtitleURL, TorrentSubtitleURL: torrentSubtitleURL,
	}
	// Upstream reloads use an existing transport and have no context argument.
	// Interrupt the transport if this seek is canceled, and disarm the callback
	// once the reload operation has completed.
	var reloadMu sync.Mutex
	reloading := true
	stopCancel := context.AfterFunc(ctx, func() {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		if reloading {
			reloading = false
			_ = sess.castClient.ForceClose()
		}
	})
	defer func() {
		reloadMu.Lock()
		if reloading && ctx.Err() != nil {
			_ = sess.castClient.ForceClose()
		}
		reloading = false
		reloadMu.Unlock()
		stopCancel()
	}()
	// The old stream may still report PLAYING while this LOAD is pending.
	// Await the actual reload result before publishing the new timeline or
	// releasing playbackMu. Cancellation closes the transport above.
	loadErr := sess.castClient.LoadMediaOnExisting(request)
	if err := ctx.Err(); err != nil {
		return err
	}
	if loadErr != nil {
		return loadErr
	}
	sess.setTimeline(mediaURL, resolvedSeconds)
	return nil
}
