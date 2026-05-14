package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type appConfig struct {
	SourceDir string `json:"source_dir"`
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
	if os.IsNotExist(err) {
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
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(configPath(), data, 0o600)
}

func runConfig(args []string) error {
	if len(args) == 0 || args[0] == "show" {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return printJSON(cfg)
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
		return printJSON(cfg)
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}
