package configstore

import (
	"errors"
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
		{"windows without home", "windows", "", map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`}, `C:\Users\alice\AppData\Roaming\woca-native-client`},
		{"windows UNC without home", "windows", "", map[string]string{"APPDATA": `\\server\share\Roaming`}, `\\server\share\Roaming\woca-native-client`},
		{"linux xdg without home", "linux", "", map[string]string{"XDG_DATA_HOME": "/data/alice"}, "/data/alice/woca-native-client"},
		{"linux fallback", "linux", "/home/alice", nil, "/home/alice/.local/share/woca-native-client"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DataDir(tt.goos, tt.home, func(key string) string { return tt.env[key] })
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
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
		{"drive-confused XDG data home", "linux", "/home/alice", map[string]string{"XDG_DATA_HOME": `C:\data`}},
		{"drive-confused POSIX home", "darwin", `C:\Users\alice`, nil},
		{"missing APPDATA", "windows", "", nil},
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

func TestDefaultPathSkipsHomeForPrimaryLocations(t *testing.T) {
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"windows APPDATA", "windows", map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`}, `C:\Users\alice\AppData\Roaming\woca-native-client\client.db`},
		{"linux XDG", "linux", map[string]string{"XDG_DATA_HOME": "/data/alice"}, "/data/alice/woca-native-client/client.db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homeCalled := false
			got, err := defaultPath(tt.goos, func() (string, error) {
				homeCalled = true
				return "", errors.New("home should not be read")
			}, func(key string) string { return tt.env[key] })
			if err != nil || got != tt.want {
				t.Fatalf("defaultPath() = %q, %v; want %q, nil", got, err, tt.want)
			}
			if homeCalled {
				t.Fatal("defaultPath() read home for an OS-specific primary location")
			}
		})
	}
}

func TestDefaultPathReadsHomeOnlyForConsumingBranches(t *testing.T) {
	for _, tt := range []struct {
		goos string
		want string
	}{
		{"darwin", "/Users/alice/Library/Application Support/woca-native-client/client.db"},
		{"linux", "/home/alice/.local/share/woca-native-client/client.db"},
	} {
		homeCalled := false
		got, err := defaultPath(tt.goos, func() (string, error) {
			homeCalled = true
			if tt.goos == "darwin" {
				return "/Users/alice", nil
			}
			return "/home/alice", nil
		}, func(string) string { return "" })
		if err != nil || got != tt.want || !homeCalled {
			t.Fatalf("defaultPath(%q) = %q, %v, homeCalled=%v", tt.goos, got, err, homeCalled)
		}
	}
}
