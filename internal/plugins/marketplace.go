package plugins

// Marketplace install support for Install's "<name>@<marketplace>"
// spelling: the source names a marketplace (a git URL or local directory
// carrying .claude-plugin/marketplace.json) and the name picks one of its
// plugins. The entry's source resolves either inside the fetched
// marketplace copy (a relative path) or to a GitHub clone URL
// ({"source":"github","repo":"owner/repo"}); anything else is an error.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marketplacePlugin is one entry of a marketplace manifest's plugins
// list. Source is kept raw because it is polymorphic: a string (a path
// relative to the marketplace root) or a source object.
type marketplacePlugin struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      json.RawMessage `json:"source"`
}

// marketplace is the decoded .claude-plugin/marketplace.json manifest.
type marketplace struct {
	Name    string              `json:"name"`
	Plugins []marketplacePlugin `json:"plugins"`
}

// lookup returns the marketplace entry named name.
func (m marketplace) lookup(name string) (marketplacePlugin, bool) {
	for _, p := range m.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return marketplacePlugin{}, false
}

// splitMarketplaceRef splits Install's "<name>@<marketplace>" spelling.
// Plain git URLs are not marketplace refs, whatever "@" they carry: the
// git@ SSH spelling and userinfo-bearing https URLs belong to the URL,
// and isGitURL sources are handled by Install's plain path.
func splitMarketplaceRef(src string) (name, market string, ok bool) {
	if isGitURL(src) {
		return "", "", false
	}
	name, market, found := strings.Cut(src, "@")
	if !found || market == "" || !validPluginName(name) {
		return "", "", false
	}
	return name, market, true
}

// validPluginName mirrors Uninstall's name rule: plugin names never carry
// path separators, ":" or a leading dot.
func validPluginName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\:`) && !strings.HasPrefix(name, ".")
}

// installFromMarketplace installs plugin name from the marketplace at
// marketSrc into root: fetch the marketplace into a scratch directory,
// resolve name via its marketplace.json, install the resolved source
// under root/name.
func installFromMarketplace(name, marketSrc, root string) (string, error) {
	tmp, err := os.MkdirTemp("", "woca-marketplace-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	marketDir, err := fetchSource(marketSrc, tmp)
	if err != nil {
		return "", err
	}
	mp, err := readMarketplace(marketDir)
	if err != nil {
		return "", err
	}
	entry, found := mp.lookup(name)
	if !found {
		available := make([]string, 0, len(mp.Plugins))
		for _, p := range mp.Plugins {
			if p.Name != "" {
				available = append(available, p.Name)
			}
		}
		if len(available) == 0 {
			available = append(available, "(none)")
		}
		return "", fmt.Errorf("plugins: marketplace %s has no plugin %q (available: %s)",
			marketSrc, name, strings.Join(available, ", "))
	}
	local, gitURL, err := resolveMarketplaceSource(marketDir, entry.Source)
	if err != nil {
		return "", fmt.Errorf("plugins: marketplace plugin %q: %w", name, err)
	}
	dst := filepath.Join(root, name)
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if local != "" {
		if err := copyTree(local, dst); err != nil {
			return "", err
		}
	} else if err := cloneInto(gitURL, dst); err != nil {
		return "", err
	}
	return validateInstalled(dst)
}

// fetchSource clones (git URL) or copies (local directory) src into a
// fresh directory under parent and returns it.
func fetchSource(src, parent string) (string, error) {
	if isGitURL(src) {
		base := strings.TrimSuffix(filepath.Base(src), ".git")
		if base == "" {
			return "", fmt.Errorf("plugins: cannot derive name from %s", src)
		}
		dst := filepath.Join(parent, base)
		if err := cloneInto(src, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("plugins: %s is not a directory", src)
	}
	dst := filepath.Join(parent, filepath.Base(abs))
	if err := copyTree(abs, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// readMarketplace reads dir/.claude-plugin/marketplace.json.
func readMarketplace(dir string) (marketplace, error) {
	path := filepath.Join(dir, ".claude-plugin", "marketplace.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return marketplace{}, fmt.Errorf("plugins: %s: no .claude-plugin/marketplace.json", dir)
		}
		return marketplace{}, err
	}
	var mp marketplace
	if err := json.Unmarshal(data, &mp); err != nil {
		return marketplace{}, fmt.Errorf("plugins: parse %s: %w", path, err)
	}
	return mp, nil
}

// resolveMarketplaceSource resolves a marketplace entry's source against
// the fetched marketplace copy at dir. A string is a path relative to the
// marketplace root ("./"-, "../"- or plainly relative); a
// {"source":"github","repo":"owner/repo"} object resolves to the GitHub
// clone URL. Exactly one of local and gitURL is set on success; anything
// else is unsupported and errors.
func resolveMarketplaceSource(dir string, raw json.RawMessage) (local, gitURL string, err error) {
	if len(raw) == 0 {
		return "", "", fmt.Errorf("missing source")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", "", fmt.Errorf("empty source")
		}
		if filepath.IsAbs(s) || strings.Contains(s, "://") {
			return "", "", fmt.Errorf("unsupported source %q (want a path relative to the marketplace or a github source object)", s)
		}
		return filepath.Join(dir, filepath.FromSlash(s)), "", nil
	}
	var obj struct {
		Source string `json:"source"`
		Repo   string `json:"repo"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", "", fmt.Errorf("source must be a relative path string or a source object")
	}
	switch obj.Source {
	case "github":
		// A repo is "owner/repo": two non-traversing segments.
		parts := strings.Split(obj.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
			parts[0] == "." || parts[1] == "." || parts[0] == ".." || parts[1] == ".." {
			return "", "", fmt.Errorf("invalid github repo %q (want owner/repo)", obj.Repo)
		}
		return "", "https://github.com/" + obj.Repo + ".git", nil
	case "":
		return "", "", fmt.Errorf("source object missing \"source\"")
	default:
		return "", "", fmt.Errorf("unsupported source %q", obj.Source)
	}
}
