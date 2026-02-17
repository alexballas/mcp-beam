package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	go2tvadapters "github.com/alex/mcp-beam/internal/adapters/go2tv"
	"github.com/alex/mcp-beam/internal/beam"
	"github.com/alex/mcp-beam/internal/buildinfo"
	"github.com/alex/mcp-beam/internal/diagnostics"
	"github.com/alex/mcp-beam/internal/discovery"
	"github.com/alex/mcp-beam/internal/lifecycle"
	"github.com/alex/mcp-beam/internal/mcpserver"
)

type selfTestOutput struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"server"`
	Go2TVAdapters struct {
		DiscoveryWired bool `json:"discovery_wired"`
		CastWired      bool `json:"cast_wired"`
		DLNAWired      bool `json:"dlna_wired"`
	} `json:"go2tv_adapters"`
	Dependencies diagnostics.DependencyReport `json:"dependencies"`
}

func main() {
	selfTest := flag.Bool("self-test", false, "run dependency and wiring diagnostics then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	bundle := go2tvadapters.NewBundle()
	diag := diagnostics.DetectDependencies()

	if *selfTest {
		out := selfTestOutput{
			Dependencies: diag,
		}
		out.Server.Name = "mcp-beam"
		out.Server.Version = buildinfo.Version
		out.Go2TVAdapters.DiscoveryWired = bundle.Discovery != nil
		out.Go2TVAdapters.CastWired = bundle.CastFactory != nil
		out.Go2TVAdapters.DLNAWired = bundle.DLNAFactory != nil

		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	runCtx, stopSignals := signal.NotifyContext(context.Background(), lifecycle.TerminationSignals()...)
	defer stopSignals()

	logLevel := parseLogLevel(os.Getenv("MCP_BEAM_LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	logger.Info(
		"mcp_server_start",
		slog.String("server", "mcp-beam"),
		slog.String("version", buildinfo.Version),
		slog.String("log_level", logLevel.String()),
	)
	discoverySvc := discovery.NewService(bundle.Discovery, runCtx)
	beamManager := beam.NewManager(discoverySvc, bundle.CastFactory, bundle.DLNAFactory)
	srv := mcpserver.New(os.Stdin, os.Stdout, mcpserver.Config{
		ServerName:          "mcp-beam",
		ServerVersion:       buildinfo.Version,
		Logger:              logger,
		LocalHardwareLister: discoverySvc,
		BeamController:      beamManager,
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- srv.Run(runCtx)
	}()

	var runErr error
	select {
	case runErr = <-runErrCh:
	case <-runCtx.Done():
		runErr = runCtx.Err()
	}
	if runErr != nil {
		logger.Warn("mcp_server_stopping", slog.String("reason", runErr.Error()))
	} else {
		logger.Info("mcp_server_stopping", slog.String("reason", "clean_eof"))
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := beamManager.Close(shutdownCtx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		fmt.Fprintf(os.Stderr, "invalid MCP_BEAM_LOG_LEVEL=%q; defaulting to info\n", raw)
		return slog.LevelInfo
	}
}
