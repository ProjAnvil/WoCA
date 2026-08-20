package configstore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const applicationName = "woca-native-client"

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir, err := DataDir(runtime.GOOS, home, os.Getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client.db"), nil
}

func DataDir(goos, home string, getenv func(string) string) (string, error) {
	if home == "" || !filepath.IsAbs(home) && goos != "windows" {
		return "", fmt.Errorf("home directory must be absolute")
	}

	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", applicationName), nil
	case "windows":
		appData := getenv("APPDATA")
		if !windowsAbsolute(appData) {
			return "", fmt.Errorf("APPDATA must be an absolute Windows path")
		}
		return windowsJoin(appData, applicationName), nil
	default:
		if xdgDataHome := getenv("XDG_DATA_HOME"); xdgDataHome != "" {
			if !filepath.IsAbs(xdgDataHome) {
				return "", fmt.Errorf("XDG_DATA_HOME must be absolute")
			}
			return filepath.Join(xdgDataHome, applicationName), nil
		}
		return filepath.Join(home, ".local", "share", applicationName), nil
	}
}

func windowsAbsolute(path string) bool {
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && isWindowsSeparator(path[2]) {
		return true
	}
	if len(path) < 5 || !isWindowsSeparator(path[0]) || !isWindowsSeparator(path[1]) || isWindowsSeparator(path[2]) {
		return false
	}
	parts := strings.FieldsFunc(path[2:], func(r rune) bool {
		return r == '\\' || r == '/'
	})
	return len(parts) >= 2 && parts[0] != "" && parts[1] != ""
}

func windowsJoin(path, element string) string {
	return strings.TrimRight(path, `\\/`) + `\` + element
}

func isWindowsSeparator(b byte) bool {
	return b == '\\' || b == '/'
}
