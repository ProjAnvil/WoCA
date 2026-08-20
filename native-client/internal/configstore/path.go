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
		separator := `\`
		if strings.Contains(appData, `\\`) {
			separator = `\\`
		}
		return appData + separator + applicationName, nil
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
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && strings.ContainsAny(path[2:3], `\\/`)
}
