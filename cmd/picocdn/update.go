package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const repo = "PiDmitrius/picocdn"

var versionRe = regexp.MustCompile(`(const version = ")(\d+)\.(\d+)\.(\d+)(")`)

func bumpPatch(srcDir string) error {
	path := filepath.Join(srcDir, "cmd", "picocdn", "main.go")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := versionRe.FindSubmatchIndex(data)
	if m == nil {
		return fmt.Errorf("version string not found in %s", path)
	}
	patch, _ := strconv.Atoi(string(data[m[8]:m[9]]))
	newVersion := fmt.Sprintf("%s%s.%s.%d%s",
		string(data[m[2]:m[3]]),
		string(data[m[4]:m[5]]),
		string(data[m[6]:m[7]]),
		patch+1,
		string(data[m[10]:m[11]]),
	)
	out := make([]byte, 0, len(data)+4)
	out = append(out, data[:m[0]]...)
	out = append(out, newVersion...)
	out = append(out, data[m[1]:]...)
	fmt.Printf("version: %s.%s.%d -> %s.%s.%d\n",
		string(data[m[4]:m[5]]), string(data[m[6]:m[7]]), patch,
		string(data[m[4]:m[5]]), string(data[m[6]:m[7]]), patch+1)
	return os.WriteFile(path, out, 0o644)
}

func latestTag() (string, error) {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Head("https://github.com/" + repo + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no releases found")
	}
	parts := strings.Split(loc, "/")
	tag := parts[len(parts)-1]
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("unexpected tag format: %s", tag)
	}
	return tag, nil
}

func downloadRelease(tag string) (string, error) {
	arch := runtime.GOARCH
	name := fmt.Sprintf("picocdn-%s-linux-%s", tag, arch)
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, name)

	fmt.Printf("downloading %s...\n", name)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	tmp, err := os.CreateTemp("", "picocdn-update-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

func runUpdate() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("cannot load config: %w", err)
	}

	if cfg.SourceDir == "" {
		tag, err := latestTag()
		if err != nil {
			return fmt.Errorf("cannot get latest version: %w", err)
		}
		fmt.Printf("latest: %s (current: %s)\n", tag, version)
		binPath, err := downloadRelease(tag)
		if err != nil {
			return err
		}
		defer os.Remove(binPath)
		return runDownloadedInstall(binPath)
	}

	if err := bumpPatch(cfg.SourceDir); err != nil {
		return fmt.Errorf("version bump failed: %w", err)
	}

	fmt.Printf("building in %s...\n", cfg.SourceDir)
	outPath := filepath.Join(cfg.SourceDir, "picocdn")
	build := exec.Command("go", "build", "-o", outPath, "./cmd/picocdn")
	build.Dir = cfg.SourceDir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	return runDownloadedInstall(outPath)
}

func runFallback(args []string) error {
	tag := ""
	if len(args) > 0 {
		tag = args[0]
	}
	if tag == "" {
		var err error
		tag, err = latestTag()
		if err != nil {
			return fmt.Errorf("cannot get latest version: %w", err)
		}
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	fmt.Printf("installing %s...\n", tag)
	binPath, err := downloadRelease(tag)
	if err != nil {
		return err
	}
	defer os.Remove(binPath)
	return runDownloadedInstall(binPath)
}

func runDownloadedInstall(binPath string) error {
	install := exec.Command(binPath, "install")
	install.Stdout = os.Stdout
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	fmt.Println("restart with: picocdn restart")
	return nil
}
