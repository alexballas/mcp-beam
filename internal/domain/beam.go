package domain

type BeamRequest struct {
	Source                string `json:"source"`
	TargetDevice          string `json:"target_device"`
	Transcode             string `json:"transcode,omitempty"`
	SubtitlesPath         string `json:"subtitles_path,omitempty"`
	StartSeconds          *int   `json:"start_seconds,omitempty"`
	TorrentFileIndex      *int   `json:"torrent_file_index,omitempty"`
	SubtitleMode          string `json:"subtitle_mode,omitempty"`
	EmbeddedSubtitleTrack *int   `json:"embedded_subtitle_track,omitempty"`
}

type BeamResult struct {
	OK          bool           `json:"ok"`
	SessionID   string         `json:"session_id"`
	DeviceID    string         `json:"device_id"`
	MediaURL    string         `json:"media_url"`
	Transcoding bool           `json:"transcoding"`
	Warnings    []string       `json:"warnings"`
	Torrent     *TorrentStatus `json:"torrent,omitempty"`
}

type InspectTorrentRequest struct {
	Source    string `json:"source"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type TorrentFile struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size_bytes"`
}

type InspectTorrentResult struct {
	OK    bool          `json:"ok"`
	Files []TorrentFile `json:"files"`
}

type TorrentStatus struct {
	FileIndex       int     `json:"file_index"`
	FileName        string  `json:"file_name"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
	ProgressPercent float64 `json:"progress_percent"`
}

type StopRequest struct {
	TargetDevice string `json:"target_device,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}

type StopResult struct {
	OK               bool     `json:"ok"`
	StoppedSessionID string   `json:"stopped_session_id"`
	DeviceID         string   `json:"device_id"`
	Warnings         []string `json:"warnings,omitempty"`
}

type PlaybackControlRequest struct {
	TargetDevice string `json:"target_device,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}

type PlaybackControlResult struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	DeviceID  string `json:"device_id"`
	State     string `json:"state"`
}

type SeekRequest struct {
	TargetDevice    string   `json:"target_device,omitempty"`
	SessionID       string   `json:"session_id,omitempty"`
	PositionSeconds *int     `json:"position_seconds,omitempty"`
	PositionPercent *float64 `json:"position_percent,omitempty"`
	FromEndSeconds  *int     `json:"from_end_seconds,omitempty"`
	DeltaSeconds    *int     `json:"delta_seconds,omitempty"`
}

type SeekResult struct {
	OK                      bool     `json:"ok"`
	SessionID               string   `json:"session_id"`
	DeviceID                string   `json:"device_id"`
	PositionSeconds         int      `json:"position_seconds"`
	RequestedMode           string   `json:"requested_mode"`
	ResolvedPositionSeconds int      `json:"resolved_position_seconds"`
	DurationSeconds         *float64 `json:"duration_seconds,omitempty"`
}

type StatusRequest struct {
	TargetDevice string `json:"target_device,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
}

type StatusResult struct {
	OK              bool           `json:"ok"`
	SessionID       string         `json:"session_id"`
	DeviceID        string         `json:"device_id"`
	DeviceName      string         `json:"device_name,omitempty"`
	Protocol        string         `json:"protocol"`
	State           string         `json:"state"`
	PositionSeconds *float64       `json:"position_seconds,omitempty"`
	DurationSeconds *float64       `json:"duration_seconds,omitempty"`
	Title           string         `json:"title,omitempty"`
	ContentType     string         `json:"content_type,omitempty"`
	MediaURL        string         `json:"media_url,omitempty"`
	Transcoding     bool           `json:"transcoding"`
	Warnings        []string       `json:"warnings,omitempty"`
	Torrent         *TorrentStatus `json:"torrent,omitempty"`
}

type ToolError struct {
	Code           string         `json:"code"`
	Message        string         `json:"message"`
	Limitations    []Limitation   `json:"limitations,omitempty"`
	SuggestedFixes []string       `json:"suggested_fixes,omitempty"`
	Details        map[string]any `json:"details,omitempty"`
}

func (e *ToolError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}
