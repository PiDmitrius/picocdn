package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PiDmitrius/picocdn/internal/auth"
)

type appConfig struct {
	SourceDir  string           `json:"source_dir,omitempty"`
	RootTokens []auth.RootToken `json:"root_tokens,omitempty"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "picocdn")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

func loadConfig() (*appConfig, error) {
	data, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return &appConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg appConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfig(cfg *appConfig) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(configDir(), ".config-*")
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
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, configPath()); err != nil {
		return err
	}
	keepTmp = true
	dir, err := os.Open(configDir())
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func runConfig(args []string) error {
	if len(args) == 0 || args[0] == "show" {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return printJSON(redactConfig(cfg))
	}
	switch args[0] {
	case "set-source-dir":
		if len(args) != 2 {
			return fmt.Errorf("usage: picocdn config set-source-dir <path|->")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if args[1] == "-" {
			cfg.SourceDir = ""
		} else {
			abs, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			cfg.SourceDir = abs
		}
		if err := saveConfig(cfg); err != nil {
			return err
		}
		return printJSON(redactConfig(cfg))
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

// redactConfig returns a view of the config suitable for printing: root token
// hashes are dropped, only ids/names/timestamps remain.
func redactConfig(cfg *appConfig) map[string]any {
	roots := make([]auth.RootTokenInfo, 0, len(cfg.RootTokens))
	for _, rt := range cfg.RootTokens {
		roots = append(roots, auth.RootTokenInfo{
			ID:        rt.ID,
			Name:      rt.Name,
			CreatedAt: rt.CreatedAt,
		})
	}
	return map[string]any{
		"source_dir":  cfg.SourceDir,
		"root_tokens": roots,
	}
}
