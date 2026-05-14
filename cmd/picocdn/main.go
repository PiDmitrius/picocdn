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

const version = "0.1.1"

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
	case "namespace":
		return runNamespace(args[1:])
	case "token":
		return runToken(args[1:])
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
	fs.StringVar(&cfg.AuthFile, "auth-file", envString("PICOCDN_AUTH_FILE", ""), "auth JSON file")
	fs.StringVar(&cfg.BaseDomain, "base-domain", envString("PICOCDN_BASE_DOMAIN", ""), "base CDN domain for namespace subdomains (empty disables subdomain routing)")
	fs.Int64Var(&cfg.MaxUploadBytes, "max-upload-bytes", envInt64("PICOCDN_MAX_UPLOAD_BYTES", 1<<30), "maximum upload request body size")
	fs.DurationVar(&cfg.ReloadInterval, "reload-interval", envDuration("PICOCDN_RELOAD_INTERVAL", 5*time.Second), "auth.json reload poll interval (0 to disable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.AuthFile == "" {
		cfg.AuthFile = filepath.Join(cfg.DataDir, "auth.json")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv, err := server.New(cfg, logger)
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

	srv.StartReloadWatcher(ctx)

	errc := make(chan error, 1)
	go func() {
		logger.Info("picocdn listening", "addr", cfg.Addr, "data_dir", cfg.DataDir, "auth_file", cfg.AuthFile, "base_domain", cfg.BaseDomain, "reload_interval", cfg.ReloadInterval)
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

func runNamespace(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing namespace subcommand")
	}
	switch args[0] {
	case "create":
		return runNamespaceCreate(args[1:])
	case "list":
		return runNamespaceList(args[1:])
	case "show":
		return runNamespaceShow(args[1:])
	case "delete":
		return runNamespaceDelete(args[1:])
	case "set-public":
		return runNamespaceSetPublic(args[1:])
	default:
		return fmt.Errorf("unknown namespace subcommand %q", args[0])
	}
}

func runNamespaceList(args []string) error {
	var authFile string
	fs := flag.NewFlagSet("namespace list", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: picocdn namespace list [--auth-file path]")
	}
	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	return printJSON(auth.ListNamespaces(authConfig))
}

func runNamespaceShow(args []string) error {
	var authFile string
	fs := flag.NewFlagSet("namespace show", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn namespace show [--auth-file path] <namespace>")
	}
	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	info, tokens, err := auth.ShowNamespace(authConfig, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"namespace": info,
		"tokens":    tokens,
	})
}

func runNamespaceDelete(args []string) error {
	var authFile string
	var force bool
	fs := flag.NewFlagSet("namespace delete", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	fs.BoolVar(&force, "force", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn namespace delete [--auth-file path] [--force] <namespace>")
	}
	if !force {
		return fmt.Errorf("refusing to delete without --force")
	}
	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	if err := auth.DeleteNamespace(authConfig, fs.Arg(0)); err != nil {
		return err
	}
	if err := auth.SaveFile(authFile, authConfig); err != nil {
		return err
	}
	return printJSON(map[string]string{
		"status":    "deleted",
		"namespace": fs.Arg(0),
	})
}

func runNamespaceSetPublic(args []string) error {
	var authFile string
	var enable, disable bool
	fs := flag.NewFlagSet("namespace set-public", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	fs.BoolVar(&enable, "on", false, "enable public-read")
	fs.BoolVar(&disable, "off", false, "disable public-read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (enable == disable) {
		return fmt.Errorf("usage: picocdn namespace set-public --on|--off [--auth-file path] <namespace>")
	}
	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	if err := auth.SetNamespacePublicRead(authConfig, fs.Arg(0), enable); err != nil {
		return err
	}
	if err := auth.SaveFile(authFile, authConfig); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"namespace":   fs.Arg(0),
		"public_read": enable,
	})
}

func runNamespaceCreate(args []string) error {
	var authFile string
	fs := flag.NewFlagSet("namespace create", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn namespace create [--auth-file path] <namespace>")
	}

	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	created, err := auth.CreateNamespace(authConfig, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := auth.SaveFile(authFile, authConfig); err != nil {
		return err
	}
	return printJSON(created)
}

func runToken(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing token subcommand")
	}
	switch args[0] {
	case "create":
		return runTokenCreate(args[1:])
	case "list":
		return runTokenList(args[1:])
	case "revoke":
		return runTokenRevoke(args[1:])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func runTokenList(args []string) error {
	var authFile string
	fs := flag.NewFlagSet("token list", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn token list [--auth-file path] <namespace>")
	}

	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	tokens, err := auth.ListTokens(authConfig, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(tokens)
}

func runTokenRevoke(args []string) error {
	var authFile string
	fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: picocdn token revoke [--auth-file path] <namespace> <token_id>")
	}

	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	if err := auth.RevokeToken(authConfig, fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	if err := auth.SaveFile(authFile, authConfig); err != nil {
		return err
	}
	return printJSON(map[string]string{
		"status":    "revoked",
		"namespace": fs.Arg(0),
		"token_id":  fs.Arg(1),
	})
}

func runTokenCreate(args []string) error {
	var authFile, name string
	var permissions stringList
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
	fs.StringVar(&name, "name", "", "token name")
	fs.Var(&permissions, "perm", "permission, can be repeated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn token create [--auth-file path] --name name --perm read <namespace>")
	}

	authConfig, err := auth.LoadFile(authFile)
	if err != nil {
		return err
	}
	created, err := auth.CreateToken(authConfig, fs.Arg(0), name, permissions)
	if err != nil {
		return err
	}
	if err := auth.SaveFile(authFile, authConfig); err != nil {
		return err
	}
	return printJSON(created)
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

func runBackup(args []string) error {
	var dataDir, authFile, output string
	var includeTmp bool
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	fs.StringVar(&dataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "storage directory")
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "auth JSON file")
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

	if err := tarAddFile(tw, authFile, "auth.json"); err != nil {
		return fmt.Errorf("backup auth.json: %w", err)
	}
	for _, sub := range []string{"blobs", "aliases"} {
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
	var dataDir, authFile, input string
	var force bool
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	fs.StringVar(&dataDir, "data-dir", envString("PICOCDN_DATA_DIR", "/var/lib/picocdn"), "destination data directory")
	fs.StringVar(&authFile, "auth-file", defaultAuthFile(), "destination auth file")
	fs.StringVar(&input, "in", "", "input path (- or empty for stdin)")
	fs.BoolVar(&force, "force", false, "allow restoring into a non-empty target")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !force {
		if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
			return fmt.Errorf("data-dir %s is not empty; pass --force to overwrite", dataDir)
		}
		if _, err := os.Stat(authFile); err == nil {
			return fmt.Errorf("auth-file %s exists; pass --force to overwrite", authFile)
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
		dest, err := restoreDest(hdr.Name, dataDir, authFile)
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
	if err := os.Chmod(authFile, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return printJSON(map[string]string{
		"status":    "restored",
		"data_dir":  dataDir,
		"auth_file": authFile,
	})
}

func restoreDest(name, dataDir, authFile string) (string, error) {
	name = filepath.ToSlash(filepath.Clean(name))
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." || strings.Contains(name, "/../") {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	if name == "auth.json" {
		return authFile, nil
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

func defaultAuthFile() string {
	if value := os.Getenv("PICOCDN_AUTH_FILE"); value != "" {
		return value
	}
	dataDir := envString("PICOCDN_DATA_DIR", "/var/lib/picocdn")
	return filepath.Join(dataDir, "auth.json")
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

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}
