package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PiDmitrius/picocdn/internal/auth"
	"github.com/PiDmitrius/picocdn/internal/server"
	"github.com/PiDmitrius/picocdn/internal/store"
)

const version = "0.3.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || args[0] == "serve" {
		if len(args) > 0 && args[0] == "serve" {
			args = args[1:]
		}
		return runServe(args)
	}

	switch args[0] {
	case "start":
		if len(args) > 1 && args[1] == "--foreground" {
			return runServe(args[2:])
		}
		return runServiceStart()
	case "stop":
		return runServiceCtl("stop")
	case "restart":
		return runServiceCtl("restart")
	case "status":
		return runStatus()
	case "install":
		return runInstall()
	case "uninstall":
		return runUninstall()
	case "update":
		return runUpdate()
	case "fallback":
		return runFallback(args[1:])
	case "config":
		return runConfig(args[1:])
	case "init":
		return runInit(args[1:])
	case "root":
		return runRoot(args[1:])
	case "gc":
		return runGC(args[1:])
	case "backup":
		return runBackup(args[1:])
	case "restore":
		return runRestore(args[1:])
	case "version":
		fmt.Printf("picocdn %s\n", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runServe(args []string) error {
	var cfg server.Config

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.Addr, "addr", envString("PICOCDN_ADDR", ":8080"), "HTTP listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "storage directory")
	fs.StringVar(&cfg.BaseDomain, "base-domain", envString("PICOCDN_BASE_DOMAIN", ""), "base CDN domain for namespace subdomains (empty disables subdomain routing)")
	fs.Int64Var(&cfg.MaxUploadBytes, "max-upload-bytes", envInt64("PICOCDN_MAX_UPLOAD_BYTES", 1<<30), "maximum upload request body size")
	if err := fs.Parse(args); err != nil {
		return err
	}

	appCfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	authStore, err := auth.NewStore(namespacesDir(cfg.DataDir), appCfg.RootTokens)
	if err != nil {
		return fmt.Errorf("load auth: %w", err)
	}
	if len(appCfg.RootTokens) == 0 {
		logger.Warn("no root tokens configured; run `picocdn init` to bootstrap")
	}
	blobStore := store.New(cfg.DataDir)

	srv, err := server.New(cfg, authStore, blobStore, logger)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("picocdn listening",
			"addr", cfg.Addr,
			"data_dir", cfg.DataDir,
			"namespaces_dir", namespacesDir(cfg.DataDir),
			"base_domain", cfg.BaseDomain,
			"root_tokens", len(appCfg.RootTokens),
		)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func namespacesDir(dataDir string) string {
	return filepath.Join(dataDir, "namespaces")
}

func runGC(args []string) error {
	var dataDir string
	var grace time.Duration
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	fs.StringVar(&dataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "storage directory")
	fs.DurationVar(&grace, "grace", time.Hour, "skip files newer than this (race safety)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := store.New(dataDir)
	deleted, freed, err := s.GC(grace)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"deleted_blobs": deleted,
		"freed_bytes":   freed,
		"grace":         grace.String(),
	})
}

// runBackup writes a gzip-tar containing config.json and data/{namespaces,blobs,aliases}.
func runBackup(args []string) error {
	var dataDir, output string
	var includeTmp bool
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	fs.StringVar(&dataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "storage directory")
	fs.StringVar(&output, "out", "", "output path (- or empty for stdout)")
	fs.BoolVar(&includeTmp, "include-tmp", false, "include tmp/ contents")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var sink io.Writer = os.Stdout
	if output != "" && output != "-" {
		f, err := os.Create(output)
		if err != nil {
			return err
		}
		defer f.Close()
		sink = f
	}
	gz := gzip.NewWriter(sink)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	if err := tarAddFile(tw, configPath(), "config.json"); err != nil {
		return fmt.Errorf("backup config.json: %w", err)
	}
	for _, sub := range []string{"namespaces", "blobs", "aliases"} {
		root := filepath.Join(dataDir, sub)
		if err := tarAddTree(tw, root, filepath.Join("data", sub)); err != nil {
			return fmt.Errorf("backup %s: %w", sub, err)
		}
	}
	if includeTmp {
		root := filepath.Join(dataDir, "tmp")
		if err := tarAddTree(tw, root, filepath.Join("data", "tmp")); err != nil {
			return fmt.Errorf("backup tmp: %w", err)
		}
	}
	return nil
}

func tarAddFile(tw *tar.Writer, src, name string) error {
	info, err := os.Stat(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return err
	}
	return nil
}

func tarAddTree(tw *tar.Writer, root, prefix string) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return tarAddFile(tw, root, prefix)
	}
	return filepath.Walk(root, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if fi.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

func runRestore(args []string) error {
	var dataDir, input string
	var force bool
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	fs.StringVar(&dataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "destination data directory")
	fs.StringVar(&input, "in", "", "input path (- or empty for stdin)")
	fs.BoolVar(&force, "force", false, "allow restoring into a non-empty target")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !force {
		if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
			return fmt.Errorf("data-dir %s is not empty; pass --force to overwrite", dataDir)
		}
		if _, err := os.Stat(configPath()); err == nil {
			return fmt.Errorf("config %s exists; pass --force to overwrite", configPath())
		}
	}

	var src io.Reader = os.Stdin
	if input != "" && input != "-" {
		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	}
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		dest, err := restoreDest(hdr.Name, dataDir, configPath())
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if isControlPlanePath(dest, dataDir) {
				if err := restoreFileAtomic(dest, 0o600, tr); err != nil {
					return err
				}
				continue
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return printJSON(map[string]string{
		"status":   "restored",
		"data_dir": dataDir,
		"config":   configPath(),
	})
}

// isControlPlanePath reports whether dest is an auth-sensitive file that must
// be written atomically: the root config or any namespace JSON.
func isControlPlanePath(dest, dataDir string) bool {
	if dest == configPath() {
		return true
	}
	nsDir := filepath.Join(dataDir, "namespaces") + string(filepath.Separator)
	if strings.HasPrefix(dest, nsDir) && strings.HasSuffix(dest, ".json") {
		return true
	}
	return false
}

// restoreFileAtomic writes r to dest via temp+fsync+chmod+rename+dir-fsync so
// a crash mid-restore never leaves a truncated control-plane file behind.
func restoreFileAtomic(dest string, mode os.FileMode, r io.Reader) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	keepTmp = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func restoreDest(name, dataDir, configFile string) (string, error) {
	name = filepath.ToSlash(filepath.Clean(name))
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." || strings.Contains(name, "/../") {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	if name == "config.json" {
		return configFile, nil
	}
	if !strings.HasPrefix(name, "data/") {
		return "", fmt.Errorf("unexpected archive path %q", name)
	}
	rel := strings.TrimPrefix(name, "data/")
	return filepath.Join(dataDir, filepath.FromSlash(rel)), nil
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
