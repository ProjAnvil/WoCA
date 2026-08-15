// Heavy shiki grammars live in this module so Vite code-splits it; it is
// dynamically imported on the first file preview instead of at startup.
import { createHighlighterCore, type HighlighterCore } from 'shiki/core'
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript'
import go from 'shiki/langs/go.mjs'
import typescript from 'shiki/langs/typescript.mjs'
import javascript from 'shiki/langs/javascript.mjs'
import tsx from 'shiki/langs/tsx.mjs'
import jsx from 'shiki/langs/jsx.mjs'
import vue from 'shiki/langs/vue.mjs'
import json from 'shiki/langs/json.mjs'
import yaml from 'shiki/langs/yaml.mjs'
import bash from 'shiki/langs/bash.mjs'
import python from 'shiki/langs/python.mjs'
import css from 'shiki/langs/css.mjs'
import html from 'shiki/langs/html.mjs'
import markdown from 'shiki/langs/markdown.mjs'
import sql from 'shiki/langs/sql.mjs'
import xml from 'shiki/langs/xml.mjs'
import rust from 'shiki/langs/rust.mjs'
import java from 'shiki/langs/java.mjs'
import toml from 'shiki/langs/toml.mjs'
import c from 'shiki/langs/c.mjs'
import cpp from 'shiki/langs/cpp.mjs'
import diff from 'shiki/langs/diff.mjs'
import githubLight from 'shiki/themes/github-light.mjs'
import githubDark from 'shiki/themes/github-dark.mjs'

export function buildHighlighter(): Promise<HighlighterCore> {
  return createHighlighterCore({
    themes: [githubLight, githubDark],
    langs: [
      go, typescript, javascript, tsx, jsx, vue, json, yaml, bash, python,
      css, html, markdown, sql, xml, rust, java, toml, c, cpp, diff,
    ],
    engine: createJavaScriptRegexEngine({ forgiving: true }),
  })
}
