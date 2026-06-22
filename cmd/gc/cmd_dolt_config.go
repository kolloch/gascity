package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newDoltConfigCmd(_ io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dolt-config",
		Short:  "Internal Dolt config helpers",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var (
		configFile   string
		host         string
		port         string
		dataDir      string
		logLevel     string
		archiveLevel int
		metricsPort  int
		cityPath     string
		scopeDir     string
		issuePrefix  string
		doltDatabase string
	)

	writeManaged := &cobra.Command{
		Use:    "write-managed",
		Short:  "Write a managed Dolt SQL config file",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := writeManagedDoltConfigFile(configFile, host, port, dataDir, logLevel, archiveLevel, metricsPort); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config write-managed: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	writeManaged.Flags().StringVar(&configFile, "file", "", "path to dolt-config.yaml")
	writeManaged.Flags().StringVar(&host, "host", "", "listener host")
	writeManaged.Flags().StringVar(&port, "port", "", "listener port")
	writeManaged.Flags().StringVar(&dataDir, "data-dir", "", "Dolt data directory")
	writeManaged.Flags().StringVar(&logLevel, "log-level", "warning", "Dolt log level")
	writeManaged.Flags().IntVar(&archiveLevel, "archive-level", 0, "Dolt auto_gc archive_level (0=off, 1=on)")
	writeManaged.Flags().IntVar(&metricsPort, "metrics-port", -1, "Dolt forensic metrics port, localhost-only (<=0 disables; break-glass only)")
	_ = writeManaged.MarkFlagRequired("file")
	_ = writeManaged.MarkFlagRequired("host")
	_ = writeManaged.MarkFlagRequired("port")
	_ = writeManaged.MarkFlagRequired("data-dir")
	cmd.AddCommand(writeManaged)

	normalizeScope := &cobra.Command{
		Use:    "normalize-scope",
		Short:  "Normalize canonical bd scope files after backend init",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cityPath == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --city") //nolint:errcheck
				return errExit
			}
			if scopeDir == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --dir") //nolint:errcheck
				return errExit
			}
			if issuePrefix == "" {
				fmt.Fprintln(stderr, "gc dolt-config normalize-scope: missing --prefix") //nolint:errcheck
				return errExit
			}
			if err := normalizeCanonicalBdScopeFilesForInit(cityPath, scopeDir, issuePrefix, doltDatabase); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config normalize-scope: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := removeScopeLocalDoltServerArtifacts(scopeDir); err != nil {
				fmt.Fprintf(stderr, "gc dolt-config normalize-scope: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	normalizeScope.Flags().StringVar(&cityPath, "city", "", "city root")
	normalizeScope.Flags().StringVar(&scopeDir, "dir", "", "scope root to normalize")
	normalizeScope.Flags().StringVar(&issuePrefix, "prefix", "", "scope issue prefix")
	normalizeScope.Flags().StringVar(&doltDatabase, "dolt-database", "", "pinned Dolt database")
	_ = normalizeScope.MarkFlagRequired("city")
	_ = normalizeScope.MarkFlagRequired("dir")
	_ = normalizeScope.MarkFlagRequired("prefix")
	cmd.AddCommand(normalizeScope)
	return cmd
}

// writeManagedDoltConfigFile writes the managed Dolt SQL server config.
//
// metricsPort arms Dolt's Prometheus metrics listener (which also exposes the
// concurrent-query/connection gauges) on localhost only. It is an off-by-default
// forensic break-glass: a positive value emits a `metrics` block so a recurring
// Dolt CPU storm can be correlated with query/connection load before the
// dolt-cpu-restart break-glass restarts the server. Values <= 0 omit the block
// entirely, leaving the default config byte-identical. The listener is pinned to
// 127.0.0.1 so the diagnostic port is never reachable off-host.
func writeManagedDoltConfigFile(path, host, port, dataDir, logLevel string, archiveLevel, metricsPort int) error {
	if path == "" {
		return fmt.Errorf("missing --file")
	}
	if host == "" {
		return fmt.Errorf("missing --host")
	}
	if port == "" {
		return fmt.Errorf("missing --port")
	}
	if dataDir == "" {
		return fmt.Errorf("missing --data-dir")
	}
	if logLevel == "" {
		logLevel = "warning"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	waitTimeout := managedDoltWaitTimeout()
	waitTimeoutLine := ""
	if waitTimeout > 0 {
		waitTimeoutLine = fmt.Sprintf("  wait_timeout: %q\n", strconv.Itoa(waitTimeout))
	}
	metricsBlock := managedDoltMetricsBlock(metricsPort)
	content := fmt.Sprintf(`# Dolt SQL server configuration — managed by gc-beads-bd
# Do not edit manually; changes are overwritten on each server start.
# To customize, set environment variables:
#   GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER, GC_DOLT_PASSWORD, GC_DOLT_LOGLEVEL,
#   GC_DOLT_METRICS_PORT (forensic break-glass; localhost-only; <=0 disables)

log_level: %s

listener:
  port: %s
  host: %s
  max_connections: 1000
  back_log: 50
  max_connections_timeout_millis: 5000
  read_timeout_millis: 300000
  write_timeout_millis: 300000

data_dir: %q

# auto_gc is disabled — dolt#10944 load-avg gating means upstream auto-GC effectively never fires.
# Compaction-driven scheduled GC replaces it. See gastownhall/gascity#1918, #1200, #1977 for context.
behavior:
  auto_gc_behavior:
    enable: false
    archive_level: %d

# Managed Gas City workloads generate short-lived probe and metadata queries.
# Dolt's persistent stats worker can make those tiny databases grow large
# stats stores and burn CPU, especially on macOS endpoint-managed machines.
# Keep stats disabled for managed servers; use explicit gc dolt maintenance
# commands for storage cleanup instead of background workers.
system_variables:
  dolt_auto_gc_enabled: "OFF"
  dolt_stats_enabled: "OFF"
  dolt_stats_gc_enabled: "OFF"
  dolt_stats_memory_only: "ON"
  dolt_stats_paused: "ON"
%s%s`, logLevel, port, host, dataDir, archiveLevel, waitTimeoutLine, metricsBlock)
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

// managedDoltMetricsBlock renders the optional `metrics` section for the managed
// Dolt config. A positive port arms Dolt's Prometheus metrics listener — bound
// to 127.0.0.1 so it is never reachable off-host — for forensic break-glass use.
// Ports <= 0 return the empty string, leaving the default config unchanged.
func managedDoltMetricsBlock(metricsPort int) string {
	if metricsPort <= 0 {
		return ""
	}
	return fmt.Sprintf(`
# Forensic metrics endpoint (Prometheus query/connection gauges), localhost-only.
# Off by default; armed via GC_DOLT_METRICS_PORT as a break-glass so a recurring
# Dolt CPU storm can be correlated with query/connection load before a restart.
metrics:
  host: 127.0.0.1
  port: %d
`, metricsPort)
}

func managedDoltWaitTimeout() int {
	const defaultWaitTimeout = 30
	raw := os.Getenv("GC_DOLT_WAIT_TIMEOUT")
	if raw == "" {
		return defaultWaitTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultWaitTimeout
	}
	if n < 0 {
		return 0
	}
	return n
}
