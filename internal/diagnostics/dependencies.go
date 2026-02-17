package diagnostics

import "os/exec"

var lookPath = exec.LookPath

type BinaryStatus struct {
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
}

type DependencyReport struct {
	FFmpeg             BinaryStatus `json:"ffmpeg"`
	FFprobe            BinaryStatus `json:"ffprobe"`
	AllRequiredPresent bool         `json:"all_required_present"`
}

func DetectDependencies() DependencyReport {
	ffmpeg := detectBinary("ffmpeg")
	ffprobe := detectBinary("ffprobe")

	return DependencyReport{
		FFmpeg:             ffmpeg,
		FFprobe:            ffprobe,
		AllRequiredPresent: ffmpeg.Found && ffprobe.Found,
	}
}

func detectBinary(name string) BinaryStatus {
	path, err := lookPath(name)
	if err != nil {
		return BinaryStatus{Found: false}
	}

	return BinaryStatus{
		Found: true,
		Path:  path,
	}
}
