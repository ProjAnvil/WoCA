// Package woca is the module root package. It embeds the built-in
// capability-plugin tree so a single woca binary can provision plugins
// without external files. (The web SPA embed lives in the separate
// web/webdist package so entries without a UI do not link it.)
package woca

import (
	"embed"
	"io/fs"
)

// plugins embeds the built-in capability-plugin tree (woca-plugins/): the
// manifests the settings UI lists, plus the python plugins' embedded scripts
// and dependency locks. Unlike web/dist this is source-managed (no build step).
//
//go:embed all:woca-plugins
var plugins embed.FS

// PluginsFS returns the embedded plugin filesystem rooted at woca-plugins.
func PluginsFS() fs.FS {
	sub, err := fs.Sub(plugins, "woca-plugins")
	if err != nil {
		panic(err)
	}
	return sub
}

// SkillsFS returns the embedded built-in skills tree (woca-plugins/skills):
// markdown method skills vendored from open-source collections, merged below
// every user-configurable layer by agentbuild.
func SkillsFS() fs.FS {
	sub, err := fs.Sub(plugins, "woca-plugins/skills")
	if err != nil {
		panic(err)
	}
	return sub
}
