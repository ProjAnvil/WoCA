package configstore

import (
	"fmt"
	"os"
	pathpkg "path"
	"runtime"
	"strings"
)

const applicationName = "woca-native-client"

func DefaultPath() (string, error) {
	return defaultPath(runtime.GOOS, os.UserHomeDir, os.Getenv)
}

func defaultPath(goos string, userHomeDir func() (string, error), getenv func(string) string) (string, error) {
	home := ""
	if requiresHome(goos, getenv) {
		var err error
		home, err = userHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
	}
	dir, err := DataDir(goos, home, getenv)
	if err != nil {
		return "", err
	}
	return joinForOS(goos, dir, "client.db"), nil
}

func DataDir(goos, home string, getenv func(string) string) (string, error) {
	switch goos {
	case "darwin":
		if !posixAbsolute(home) {
			return "", fmt.Errorf("home directory must be an absolute POSIX path")
		}
		return pathpkg.Join(home, "Library", "Application Support", applicationName), nil
	case "windows":
		appData := getenv("APPDATA")
		if !windowsAbsolute(appData) {
			return "", fmt.Errorf("APPDATA must be an absolute Windows path")
		}
		return windowsJoin(appData, applicationName), nil
	default:
		if xdgDataHome := getenv("XDG_DATA_HOME"); xdgDataHome != "" {
			if !posixAbsolute(xdgDataHome) {
				return "", fmt.Errorf("XDG_DATA_HOME must be an absolute POSIX path")
			}
			return pathpkg.Join(xdgDataHome, applicationName), nil
		}
		if !posixAbsolute(home) {
			return "", fmt.Errorf("home directory must be an absolute POSIX path")
		}
		return pathpkg.Join(home, ".local", "share", applicationName), nil
	}
}

func requiresHome(goos string, getenv func(string) string) bool {
	return goos == "darwin" || goos != "windows" && getenv("XDG_DATA_HOME") == ""
}

func joinForOS(goos string, elements ...string) string {
	if goos == "windows" {
		joined := elements[0]
		for _, element := range elements[1:] {
			joined = windowsJoin(joined, element)
		}
		return joined
	}
	return pathpkg.Join(elements...)
}

func posixAbsolute(path string) bool {
	return strings.HasPrefix(path, "/")
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
