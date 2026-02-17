package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/alex/mcp-beam/internal/buildinfo"
	"github.com/alex/mcp-beam/internal/release"
)

func main() {
	outDir := flag.String("out", "dist", "output directory for release artifacts")
	flag.Parse()

	artifacts, err := release.BuildArtifacts(context.Background(), release.Options{
		OutDir:   *outDir,
		RepoRoot: ".",
		Version:  buildinfo.Version,
		Targets:  release.DefaultTargets,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for _, artifact := range artifacts {
		fmt.Println(artifact.ArchiveName)
	}
	fmt.Println("SHA256SUMS")
}
