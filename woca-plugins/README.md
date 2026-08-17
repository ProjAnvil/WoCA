# woca-plugins

Source tree of woca's built-in **capability plugins**, embedded into the
binary via `go:embed` (root package `pluginsfs.go`). These are distinct from
the skills/agents plugins installed under `~/.woca/plugins`
(`internal/plugins`, marketplace-style distribution): the plugins here give
the agent **tools** and ship with each woca release.

```
golang/<name>/   # Go plugin: implementation lives in internal/pluginrt; this tree holds plugin.json + docs
python/<name>/   # Python plugin: embedded scripts under scripts/ + locked requirements
```

## plugin.json fields

| Field | Meaning |
| --- | --- |
| name | plugin name (referenced by the `builtin-plugins` setting key) |
| kind | `golang` \| `python` |
| version | plugin's own version |
| description | shown in the settings panel |
| tools | agent tool names contributed (also registered in permission categories) |
| runtime | `pinchtab` \| `python` — the runtime the install button provisions |
| scripts | (python) scripts to materialize and execute, relative to the plugin dir |
| requirements | (python) locked requirements file, relative to the plugin dir |

Adding a plugin: create the directory and manifest. Go plugins additionally
need their tools implemented in `internal/pluginrt` and registered in
`tools.go` / `registry.go` (permission categories); Python plugins become
available automatically via `runtime/python`.

On-disk layout: shared heavy assets live under `~/.woca/plugins/runtimes/`
(uv/CPython/pinchtab, shared across plugins); each Python plugin gets an
isolated environment under `~/.woca/plugins/<name>/` (venv/scripts/ENV_OK —
delete the directory to uninstall). Marketplace plugins (skills/agents) share
the `~/.woca/plugins` root; the loader only picks up directories containing a
`plugin.json`.
