package configstore

import (
	"path/filepath"
	"testing"
)

func TestDataDir(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		env  map[string]string
		want string
	}{
		{"darwin", "darwin", "/Users/alice", nil, "/Users/alice/Library/Application Support/woca-native-client"},
		{"windows", "windows", `C:\\Users\\alice`, map[string]string{"APPDATA": `C:\\Users\\alice\\AppData\\Roaming`}, `C:\\Users\\alice\\AppData\\Roaming\\woca-native-client`},
		{"linux xdg", "linux", "/home/alice", map[string]string{"XDG_DATA_HOME": "/data/alice"}, "/data/alice/woca-native-client"},
		{"linux fallback", "linux", "/home/alice", nil, "/home/alice/.local/share/woca-native-client"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DataDir(tt.goos, tt.home, func(key string) string { return tt.env[key] })
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Clean(got) != filepath.Clean(tt.want) {
				t.Fatalf("DataDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDataDirRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		env  map[string]string
	}{
		{"relative XDG data home", "linux", "/home/alice", map[string]string{"XDG_DATA_HOME": "relative"}},
		{"missing APPDATA", "windows", `C:\\Users\\alice`, nil},
		{"missing home", "darwin", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DataDir(tt.goos, tt.home, func(key string) string { return tt.env[key] })
			if err == nil {
				t.Fatal("DataDir() error = nil, want error")
			}
		})
	}
}
