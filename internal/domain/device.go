package domain

type Device struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Address      string       `json:"address"`
	IsAudioOnly  bool         `json:"is_audio_only"`
	Protocol     string       `json:"protocol"`
	Capabilities Capabilities `json:"capabilities"`
}

type Capabilities struct {
	SupportsFileSource bool         `json:"supports_file_source"`
	SupportsURLSource  bool         `json:"supports_url_source"`
	SupportsHLSM3U8URL bool         `json:"supports_hls_m3u8_url"`
	Limitations        []Limitation `json:"limitations"`
}

type Limitation struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
