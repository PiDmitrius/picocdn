package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runServiceStart() error {
	cmd := exec.Command("systemctl", "--user", "start", "picocdn")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed: %w; try 'picocdn install' first, or 'picocdn start --foreground'", err)
	}
	fmt.Println("picocdn started")
	return nil
}

func runServiceCtl(action string) error {
	cmd := exec.Command("systemctl", "--user", action, "picocdn")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runStatus() error {
	cmd := exec.Command("systemctl", "--user", "status", "picocdn", "--no-pager", "-l")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}

	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(binDir, "picocdn")
	if err := copyFile(exe, dst, 0o755); err != nil {
		return fmt.Errorf("cannot install binary: %w", err)
	}
	fmt.Printf("installed: %s\n", tildePath(dst))

	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "picocdn"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(configPath()); os.IsNotExist(err) {
		if err := saveConfig(&appConfig{}); err != nil {
			return err
		}
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, "picocdn.service")
	unit := renderServiceUnit(dst)
	if drifted, err := unitDrifted(unitPath, unit); err != nil {
		return fmt.Errorf("cannot inspect current service unit: %w", err)
	} else if drifted {
		fmt.Fprintf(os.Stderr, "warning: local systemd unit drift detected, overwriting %s\n", tildePath(unitPath))
	}
	if err := verifyServiceUnit(unit); err != nil {
		if ignorableVerifyError(err) {
			fmt.Fprintf(os.Stderr, "warning: service unit verification skipped: %v\n", err)
		} else {
			return fmt.Errorf("service unit verification failed: %w", err)
		}
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("cannot install service unit: %w", err)
	}
	fmt.Printf("installed: %s\n", tildePath(unitPath))

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	_ = exec.Command("systemctl", "--user", "enable", "picocdn").Run()

	if serviceIsActive() {
		fmt.Println("\nInstalled. Service is running; restart it with: picocdn restart")
	} else {
		fmt.Println("\nInstalled. Run: picocdn start")
	}
	return nil
}

func runUninstall() error {
	_ = exec.Command("systemctl", "--user", "stop", "picocdn").Run()
	_ = exec.Command("systemctl", "--user", "disable", "picocdn").Run()
	home, _ := os.UserHomeDir()
	_ = os.Remove(filepath.Join(home, ".config", "systemd", "user", "picocdn.service"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("uninstalled")
	return nil
}

func serviceIsActive() bool {
	out, _ := exec.Command("systemctl", "--user", "is-active", "picocdn").Output()
	return strings.TrimSpace(string(out)) == "active"
}

func serviceNextCommand() string {
	if serviceIsActive() {
		return "restart with: picocdn restart"
	}
	return "start with: picocdn start"
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.WriteFile(dst, data, mode)
}

func renderServiceUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=picocdn
After=network.target
StartLimitBurst=3
StartLimitIntervalSec=60

[Service]
Type=simple
Environment=PICOCDN_ADDR=127.0.0.1:8080
Environment=PICOCDN_DATA_DIR=%%h/.local/share/picocdn
Environment=PICOCDN_AUTH_FILE=%%h/.local/share/picocdn/auth.json
EnvironmentFile=-%%h/.config/picocdn/picocdn.env
ExecStart=%s start --foreground
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, binPath)
}

func unitDrifted(path, expected string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !bytes.Equal(data, []byte(expected)), nil
}

func verifyServiceUnit(unit string) error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		return fmt.Errorf("systemd-analyze not found")
	}

	tmp, err := os.CreateTemp("", "picocdn-service-*.service")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(unit); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	cmd := exec.Command("systemd-analyze", "verify", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func ignorableVerifyError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "SO_PASSCRED failed")
}

func tildePath(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
