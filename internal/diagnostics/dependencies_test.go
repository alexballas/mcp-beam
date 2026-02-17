package diagnostics

import (
	"errors"
	"testing"
)

func TestDetectDependencies(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() {
		lookPath = orig
	})

	lookPath = func(file string) (string, error) {
		switch file {
		case "ffmpeg":
			return "/usr/bin/ffmpeg", nil
		case "ffprobe":
			return "", errors.New("not found")
		default:
			return "", errors.New("not found")
		}
	}

	report := DetectDependencies()
	if !report.FFmpeg.Found {
		t.Fatal("expected ffmpeg to be found")
	}
	if report.FFmpeg.Path != "/usr/bin/ffmpeg" {
		t.Fatalf("unexpected ffmpeg path: %s", report.FFmpeg.Path)
	}
	if report.FFprobe.Found {
		t.Fatal("expected ffprobe to be missing")
	}
	if report.AllRequiredPresent {
		t.Fatal("expected AllRequiredPresent to be false")
	}
}
