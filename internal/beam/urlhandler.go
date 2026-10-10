package beam

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"time"

	"go2tv.app/go2tv/v2/httphandlers"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/go2tv/v2/utils"
)

// This inert reader exposes a URL to ffprobe without consuming a renderer body.
type urlProbeReader struct{ sourceURL string }

func (r *urlProbeReader) URL() string                  { return r.sourceURL }
func (*urlProbeReader) Read([]byte) (int, error)       { return 0, io.EOF }
func (*urlProbeReader) Seek(int64, int) (int64, error) { return 0, nil }
func (*urlProbeReader) Close() error                   { return nil }

func optionalURLMediaDuration(ctx context.Context, ffmpegPath, sourceURL string) (float64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	duration, err := utils.DurationForMediaReaderSeconds(probeCtx, ffmpegPath, &urlProbeReader{sourceURL: sourceURL})
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, nil
	}
	return duration, nil
}

// Every renderer request owns its FFmpeg process and reopens the original URL.
// Passing a URL instead of a consumed HTTP body also preserves input seeking.
func (m *Manager) chromecastURLHandler(sourceURL string, opts *utils.TranscodeOptions) http.Handler {
	return urlTranscodeHandler(sourceURL, false, func(*http.Request) *utils.TranscodeOptions { return opts })
}

func dlnaURLTranscodeHandler(sourceURL string, payload *soapcalls.TVPayload) http.Handler {
	return urlTranscodeHandler(sourceURL, true, func(r *http.Request) *utils.TranscodeOptions {
		if opts := httphandlers.DLNATranscodeOptionsForRequest(r); opts != nil {
			return opts
		}
		return payload.TranscodeOptionsSnapshot()
	})
}

func urlTranscodeHandler(sourceURL string, dlna bool, options func(*http.Request) *utils.TranscodeOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType := "video/mp4"
		if dlna {
			mediaType = utils.DLNAResourceMediaType("video/mp4", true)
		}
		setURLMediaHeaders(w.Header(), mediaType, utils.DLNAContentFeaturesOptions{Converted: true})
		w.Header().Set("Accept-Ranges", "none")
		if !allowURLMediaMethod(w, r) || r.Method == http.MethodHead {
			return
		}
		// Streamed conversion has no stable byte offsets; the receiver reloads
		// a route with the newly selected source offset when MCP seeks.
		var command exec.Cmd
		out := &urlStreamWriter{ResponseWriter: w}
		opts := options(r)
		var err error
		if dlna {
			err = utils.ServeDLNATranscodedStream(r.Context(), out, sourceURL, &command, opts)
		} else {
			err = utils.ServeChromecastTranscodedStream(r.Context(), out, sourceURL, &command, opts)
		}
		if err != nil && r.Context().Err() == nil {
			if !out.written {
				http.Error(w, "media conversion failed", http.StatusBadGateway)
			}
			if opts != nil {
				opts.Log().Error("URL media conversion failed", "error", err)
			}
		}
	})
}

type urlStreamWriter struct {
	http.ResponseWriter
	written bool
}

func (w *urlStreamWriter) Write(data []byte) (int, error) {
	w.written = true
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// Native DLNA fallback opens a fresh request-owned upstream body for GET/HEAD.
// ReverseProxy preserves Range and conditional headers and closes every body.
func nativeURLProxy(sourceURL, mediaType string) (http.Handler, io.Closer, error) {
	target, err := url.Parse(sourceURL)
	if err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          8, MaxIdleConnsPerHost: 2,
		ForceAttemptHTTP2: true, DisableCompression: true,
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			// Renderer requests target the token route, not the source path.
			req.Out.URL.Path, req.Out.URL.RawPath = target.Path, target.RawPath
			req.Out.URL.RawQuery = target.RawQuery
			if target.User != nil {
				password, _ := target.User.Password()
				req.Out.SetBasicAuth(target.User.Username(), password)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			typeOnWire := response.Header.Get("Content-Type")
			if typeOnWire == "" {
				typeOnWire = mediaType
			}
			seekable := response.StatusCode == http.StatusPartialContent || response.Header.Get("Accept-Ranges") == "bytes"
			setURLMediaHeaders(response.Header, typeOnWire, utils.DLNAContentFeaturesOptions{ByteSeek: seekable})
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() == nil {
				http.Error(w, "media source unavailable", http.StatusBadGateway)
			}
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setURLMediaHeaders(w.Header(), mediaType, utils.DLNAContentFeaturesOptions{})
		if allowURLMediaMethod(w, r) {
			proxy.ServeHTTP(w, r)
		}
	})
	return handler, &urlProxyCloser{transport: transport}, nil
}

type urlProxyCloser struct{ transport *http.Transport }

func (c *urlProxyCloser) Close() error {
	c.transport.CloseIdleConnections()
	return nil
}

func setURLMediaHeaders(header http.Header, mediaType string, features utils.DLNAContentFeaturesOptions) {
	header.Set("Content-Type", mediaType)
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Range, Content-Type, getcontentFeatures.dlna.org, transferMode.dlna.org")
	header.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	header.Set("transferMode.dlna.org", utils.DLNATransferMode(mediaType))
	header.Set("contentFeatures.dlna.org", utils.BuildDLNAContentFeatures(features))
	header.Set("realTimeInfo.dlna.org", "DLNA.ORG_TLAG=*")
}

func allowURLMediaMethod(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return false
}
