package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Target struct {
	GOOS   string
	GOARCH string
}

type Artifact struct {
	Target         Target
	ArchiveName    string
	ArchivePath    string
	PackageDirName string
}

type Options struct {
	OutDir   string
	RepoRoot string
	Version  string
	Targets  []Target
}

var DefaultTargets = []Target{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
}

func BuildArtifacts(ctx context.Context, opts Options) ([]Artifact, error) {
	if strings.TrimSpace(opts.OutDir) == "" {
		return nil, fmt.Errorf("out dir is required")
	}
	if strings.TrimSpace(opts.RepoRoot) == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	if strings.TrimSpace(opts.Version) == "" {
		return nil, fmt.Errorf("version is required")
	}
	targets := opts.Targets
	if len(targets) == 0 {
		targets = DefaultTargets
	}

	repoRoot, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repo root: %w", err)
	}
	outDir, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("resolve out dir: %w", err)
	}

	if err := os.RemoveAll(outDir); err != nil {
		return nil, fmt.Errorf("clean out dir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create out dir: %w", err)
	}

	stageRoot := filepath.Join(outDir, ".stage")
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create stage dir: %w", err)
	}
	defer os.RemoveAll(stageRoot)

	var artifacts []Artifact
	for _, target := range targets {
		pkgDirName := packageDirName(opts.Version, target)
		pkgDir := filepath.Join(stageRoot, pkgDirName)
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			return nil, fmt.Errorf("create package dir %s: %w", pkgDirName, err)
		}

		binName := binaryName(target.GOOS)
		binPath := filepath.Join(pkgDir, binName)
		if err := buildBinary(ctx, repoRoot, target, binPath); err != nil {
			return nil, err
		}

		if err := copyReleaseDocs(repoRoot, pkgDir); err != nil {
			return nil, err
		}

		archiveName := archiveName(opts.Version, target)
		archivePath := filepath.Join(outDir, archiveName)
		if target.GOOS == "windows" {
			if err := createZip(archivePath, pkgDir); err != nil {
				return nil, fmt.Errorf("create %s: %w", archiveName, err)
			}
		} else {
			if err := createTarGz(archivePath, pkgDir); err != nil {
				return nil, fmt.Errorf("create %s: %w", archiveName, err)
			}
		}

		artifacts = append(artifacts, Artifact{
			Target:         target,
			ArchiveName:    archiveName,
			ArchivePath:    archivePath,
			PackageDirName: pkgDirName,
		})
	}

	if err := writeChecksums(outDir, artifacts); err != nil {
		return nil, err
	}

	return artifacts, nil
}

func packageDirName(version string, target Target) string {
	return fmt.Sprintf("mcp-beam_%s_%s_%s", version, target.GOOS, target.GOARCH)
}

func archiveName(version string, target Target) string {
	if target.GOOS == "windows" {
		return fmt.Sprintf("mcp-beam_%s_%s_%s.zip", version, target.GOOS, target.GOARCH)
	}
	return fmt.Sprintf("mcp-beam_%s_%s_%s.tar.gz", version, target.GOOS, target.GOARCH)
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "mcp-beam.exe"
	}
	return "mcp-beam"
}

func buildBinary(ctx context.Context, repoRoot string, target Target, outPath string) error {
	args := []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", outPath, "."}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build %s/%s failed: %w: %s", target.GOOS, target.GOARCH, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyReleaseDocs(repoRoot, pkgDir string) error {
	files := [][2]string{
		{"README.md", "README.md"},
		{"mcp-beam.png", "mcp-beam.png"},
	}
	for _, pair := range files {
		src := filepath.Join(repoRoot, pair[0])
		dst := filepath.Join(pkgDir, pair[1])
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", pair[0], err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		return err
	}
	return dstFile.Close()
}

func createTarGz(archivePath, dir string) error {
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzw := gzip.NewWriter(file)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	parent := filepath.Dir(dir)
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		_, err = io.Copy(tw, src)
		return err
	})
}

func createZip(archivePath, dir string) error {
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	zw := zip.NewWriter(file)
	defer zw.Close()

	parent := filepath.Dir(dir)
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		_, err = io.Copy(writer, src)
		return err
	})
}

func writeChecksums(outDir string, artifacts []Artifact) error {
	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].ArchiveName < artifacts[j].ArchiveName
	})

	var lines []string
	for _, artifact := range artifacts {
		data, err := os.ReadFile(artifact.ArchivePath)
		if err != nil {
			return fmt.Errorf("read artifact for checksum %s: %w", artifact.ArchiveName, err)
		}
		sum := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf("%x  %s", sum, artifact.ArchiveName))
	}
	payload := strings.Join(lines, "\n") + "\n"
	checksumPath := filepath.Join(outDir, "SHA256SUMS")
	if err := os.WriteFile(checksumPath, []byte(payload), 0o644); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	return nil
}
