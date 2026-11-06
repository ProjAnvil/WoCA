package plugins

// Marketplace install ("<name>@<marketplace>") and disabled-plugin
// filtering (LoadFiltered / LoadWithSkipped), all filesystem-only.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// makeMarketplace writes a local marketplace fixture: a root carrying
// .claude-plugin/marketplace.json that lists "toolkit" with a relative
// source, plus the toolkit plugin under plugins/toolkit.
func makeMarketplace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePluginFile(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"),
		`{"name":"acme-market","plugins":[`+
			`{"name":"toolkit","source":"./plugins/toolkit","description":"ACME toolkit"}`+
			`]}`)
	plugin := filepath.Join(dir, "plugins", "toolkit")
	writePluginFile(t, filepath.Join(plugin, "plugin.json"), `{"name":"toolkit","version":"1.0.0"}`)
	writePluginFile(t, filepath.Join(plugin, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: Deploys things\n---\nRun the deploy.\n")
	return dir
}

func TestInstallFromLocalMarketplace(t *testing.T) {
	fixture := makeMarketplace(t)
	root := filepath.Join(t.TempDir(), "plugins")

	path, err := Install("toolkit@"+fixture, root)
	if err != nil {
		t.Fatalf("marketplace install: %v", err)
	}
	if filepath.Base(path) != "toolkit" {
		t.Fatalf("installed at %s; want the marketplace plugin name", path)
	}
	p, err := LoadPlugin(path)
	if err != nil || p.Manifest.Name != "toolkit" {
		t.Fatalf("installed plugin = %+v err=%v", p, err)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "toolkit-deploy" {
		t.Fatalf("skills = %+v (want the toolkit plugin's deploy skill)", p.Skills)
	}

	// The installed root loads it like any other plugin.
	list, errs := Load(root)
	if len(errs) != 0 || len(list) != 1 || list[0].Manifest.Name != "toolkit" {
		t.Fatalf("Load = %+v %v", list, errs)
	}

	// Reinstall replaces cleanly.
	if _, err := Install("toolkit@"+fixture, root); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if list, errs := Load(root); len(errs) != 0 || len(list) != 1 {
		t.Fatalf("Load after reinstall = %+v %v", list, errs)
	}
}

func TestInstallMarketplaceUnknownPlugin(t *testing.T) {
	fixture := makeMarketplace(t)
	root := filepath.Join(t.TempDir(), "plugins")

	_, err := Install("nope@"+fixture, root)
	if err == nil {
		t.Fatal("expected an error for an unknown marketplace plugin")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "toolkit") {
		t.Fatalf("err = %v; want the requested name and the available names listed", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("failed marketplace install left %d entries behind", len(entries))
	}
}

func TestInstallMarketplaceMissingMarketplaceJSON(t *testing.T) {
	// A plain plugin directory: a fine Install source, but not a marketplace.
	src := makePlugin(t, t.TempDir(), "acme")
	root := filepath.Join(t.TempDir(), "plugins")

	_, err := Install("acme@"+src, root)
	if err == nil || !strings.Contains(err.Error(), "marketplace.json") {
		t.Fatalf("err = %v; want a missing marketplace.json error", err)
	}
}

func TestMarketplaceSourceResolution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "market")
	cases := []struct {
		name    string
		source  string
		local   string
		gitURL  string
		wantErr bool
	}{
		{"dot-relative", `"./plugins/toolkit"`, filepath.Join(dir, "plugins", "toolkit"), "", false},
		{"plain-relative", `"plugins/toolkit"`, filepath.Join(dir, "plugins", "toolkit"), "", false},
		{"parent-relative", `"../shared/toolkit"`, filepath.Join(dir, "..", "shared", "toolkit"), "", false},
		// The GitHub source object resolves to a clone URL (unit-tested
		// here only: the install path would hit the network).
		{"github", `{"source":"github","repo":"owner/repo"}`, "", "https://github.com/owner/repo.git", false},
		{"absolute-path", `"/opt/plugins/x"`, "", "", true},
		{"url-string", `"https://github.com/x/y.git"`, "", "", true},
		{"empty-string", `""`, "", "", true},
		{"other-host", `{"source":"gitlab","repo":"x/y"}`, "", "", true},
		{"missing-kind", `{"repo":"x/y"}`, "", "", true},
		{"bad-repo", `{"source":"github","repo":"repo"}`, "", "", true},
		{"multi-segment-repo", `{"source":"github","repo":"a/b/c"}`, "", "", true},
		{"not-a-source", `42`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local, gitURL, err := resolveMarketplaceSource(dir, json.RawMessage(tc.source))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveMarketplaceSource(%s) = (%q, %q); want error", tc.source, local, gitURL)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if local != tc.local || gitURL != tc.gitURL {
				t.Fatalf("resolved = (%q, %q); want (%q, %q)", local, gitURL, tc.local, tc.gitURL)
			}
		})
	}
}

func TestSplitMarketplaceRef(t *testing.T) {
	cases := []struct {
		src    string
		name   string
		market string
		ok     bool
	}{
		{"toolkit@/opt/markets/acme", "toolkit", "/opt/markets/acme", true},
		{"toolkit@./market", "toolkit", "./market", true},
		{"toolkit@https://github.com/acme/market", "toolkit", "https://github.com/acme/market", true},
		// Plain git URLs are not marketplace refs, whatever @ they carry.
		{"https://github.com/acme/market", "", "", false},
		{"git@github.com:acme/market.git", "", "", false},
		{"https://user:token@github.com/acme/market.git", "", "", false},
		// Plain paths without a valid name@ prefix are unchanged.
		{"/plain/local/path", "", "", false},
		{"./relative/path", "", "", false},
		{"relative/path", "", "", false},
		// A name must be a valid plugin name and the source non-empty.
		{"@market", "", "", false},
		{"toolkit@", "", "", false},
		{"a/b@market", "", "", false},
		{".hidden@market", "", "", false},
	}
	for _, tc := range cases {
		name, market, ok := splitMarketplaceRef(tc.src)
		if ok != tc.ok || name != tc.name || market != tc.market {
			t.Fatalf("splitMarketplaceRef(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tc.src, name, market, ok, tc.name, tc.market, tc.ok)
		}
	}
}

func TestLoadFilteredSkipsDisabled(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "toolkit")
	makePlugin(t, root, "other")

	// Nothing disabled: Load and LoadFiltered agree.
	all, errs := Load(root)
	if len(errs) != 0 || len(all) != 2 {
		t.Fatalf("Load = %+v %v", all, errs)
	}

	got, errs := LoadFiltered(root, []string{"toolkit"})
	if len(errs) != 0 || len(got) != 1 || got[0].Manifest.Name != "other" {
		t.Fatalf("LoadFiltered = %+v %v (toolkit must be skipped)", got, errs)
	}

	// The skipped set is observable for /plugins.
	got, skipped, errs := LoadWithSkipped(root, []string{"toolkit"})
	if len(errs) != 0 || len(got) != 1 || !slices.Equal(skipped, []string{"toolkit"}) {
		t.Fatalf("LoadWithSkipped = %+v skipped=%v errs=%v", got, skipped, errs)
	}

	// Load is LoadFiltered with nothing disabled.
	got, errs = LoadFiltered(root, nil)
	if len(errs) != 0 || len(got) != 2 {
		t.Fatalf("LoadFiltered(root, nil) = %+v %v; want everything", got, errs)
	}
}

// A disabled name matches the manifest name too, not just the directory
// name: /plugins shows manifest names.
func TestLoadFilteredMatchesManifestName(t *testing.T) {
	root := t.TempDir()
	makePlugin(t, root, "tkdir")
	writePluginFile(t, filepath.Join(root, "tkdir", "plugin.json"), `{"name":"toolkit"}`)

	for _, name := range []string{"toolkit", "tkdir"} {
		got, skipped, errs := LoadWithSkipped(root, []string{name})
		if len(errs) != 0 || len(got) != 0 {
			t.Fatalf("disable by %q: got=%+v errs=%v", name, got, errs)
		}
		if len(skipped) != 1 {
			t.Fatalf("disable by %q: skipped=%v; want one skipped plugin", name, skipped)
		}
	}

	// A disabled plugin with a broken manifest is skipped entirely: it
	// reports no error (unlike an enabled broken plugin, which does).
	writePluginFile(t, filepath.Join(root, "tkdir", "plugin.json"), `{`)
	_, skipped, errs := LoadWithSkipped(root, []string{"tkdir"})
	if len(errs) != 0 || !slices.Equal(skipped, []string{"tkdir"}) {
		t.Fatalf("disabled broken plugin: skipped=%v errs=%v; want silent skip", skipped, errs)
	}
}
