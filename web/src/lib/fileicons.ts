// File-type icons for the workspace tree, powered by the vscode-icons
// collection (VS Code's own file-icon theme, MIT). Colors are baked into
// each SVG path, so no runtime theming is needed.
//
// The collection JSON (~3.7MB) is dynamically imported so Vite emits it as
// a separate lazy chunk — downloaded once when the tree first renders,
// never part of the main bundle (same pattern as the shiki engine chunk).

interface IconifyIcon {
  body: string
  // Iconify JSON format: per-icon viewBox is `0 0 width height`, defaulting
  // to 16 when absent. Bodies are authored in these coordinates — a few
  // icons (e.g. file-type-go at 254.5x225) are nowhere near 32x32.
  width?: number
  height?: number
}

export interface FileIconData {
  name: string
  body: string
  width: number
  height: number
}

/** vscode-icons name without the `file-type-` prefix, or null if unknown. */
export function fileIconName(fileName: string): string | null {
  const lower = fileName.toLowerCase()
  return EXACT_NAMES[lower] ?? EXT_ICONS[extensionOf(lower)]
}

/**
 * Kick off (once) loading the icon collection; resolves when icons are
 * renderable. Safe to call from every component instance.
 */
export function loadFileIcons(): Promise<void> {
  if (!pending) {
    pending = import('@iconify-json/vscode-icons/icons.json').then((m) => {
      collection = (m.default as { icons: Record<string, IconifyIcon> }).icons
    })
  }
  return pending
}

/** SVG data for a vscode-icons name, or null until loaded/unknown. */
export function fileIconBody(name: string): FileIconData | null {
  const icon = collection?.[`file-type-${name}`]
  if (!icon) return null
  return {
    name,
    body: icon.body,
    width: icon.width ?? 16,
    height: icon.height ?? 16,
  }
}

// vscode-icons paths routinely overflow their declared viewBox (coordinates
// beyond `width`/`height`, even negative), so a declared viewBox clips icons.
// Instead, each icon's real content bounds are measured in the browser via
// getBBox() on first render and cached by name — every icon then renders
// complete and at a consistent visual size.
const viewBoxCache = new Map<string, string>()

/**
 * ViewBox string for a rendered icon. `svg` must already contain the icon
 * body in the DOM; pass the declared size as fallback for the rare case
 * where measurement is impossible (e.g. hidden ancestor → 0-sized bbox).
 */
export function resolveViewBox(
  name: string,
  svg: SVGSVGElement,
  fallbackWidth: number,
  fallbackHeight: number,
): string {
  const cached = viewBoxCache.get(name)
  if (cached) return cached
  const b = svg.getBBox()
  if (!isFinite(b.width) || b.width <= 0 || !isFinite(b.height) || b.height <= 0) {
    return `0 0 ${fallbackWidth} ${fallbackHeight}`
  }
  const vb = `${b.x} ${b.y} ${b.width} ${b.height}`
  viewBoxCache.set(name, vb)
  return vb
}

function extensionOf(lowerName: string): string {
  const dot = lowerName.lastIndexOf('.')
  return dot > 0 ? lowerName.slice(dot + 1) : ''
}

// Compound extensions first so `foo.tar.gz` matches `tar.gz`, not `gz`.
const EXT_ICONS: Record<string, string> = {
  'tar.gz': 'zip',
  'tar.bz2': 'zip',
  'tar.xz': 'zip',
  'min.js': 'js-official',
  'min.css': 'css',
  'd.ts': 'typescript',
  'config.js': 'config',
  'config.ts': 'config',

  // languages
  js: 'js-official',
  mjs: 'js-official',
  cjs: 'js-official',
  jsx: 'js-official',
  ts: 'typescript',
  mts: 'typescript',
  cts: 'typescript',
  tsx: 'typescript',
  vue: 'vue',
  svelte: 'svelte',
  astro: 'astro',
  md: 'markdown',
  mdx: 'mdx',
  json: 'json-official',
  jsonc: 'json-official',
  yml: 'yaml',
  yaml: 'yaml',
  toml: 'toml',
  xml: 'xml',
  html: 'html',
  htm: 'html',
  css: 'css',
  scss: 'scss',
  sass: 'sass',
  less: 'less',
  styl: 'stylus',
  pcss: 'postcss',
  go: 'go',
  rs: 'rust',
  py: 'python',
  pyi: 'python',
  rb: 'ruby',
  php: 'php',
  java: 'java',
  kt: 'kotlin',
  kts: 'kotlin',
  swift: 'swift',
  c: 'c',
  h: 'c',
  cpp: 'cpp',
  cc: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  hh: 'cpp',
  cs: 'csharp',
  m: 'objectivec',
  mm: 'objectivec',
  scala: 'scala',
  sbt: 'scala',
  lua: 'lua',
  pl: 'perl',
  pm: 'perl',
  r: 'r',
  ex: 'elixir',
  exs: 'elixir',
  erl: 'erlang',
  hrl: 'erlang',
  hs: 'haskell',
  clj: 'clojure',
  cljs: 'clojure',
  edn: 'clojure',
  dart: 'dartlang',
  zig: 'zig',
  jl: 'julia',
  groovy: 'groovy',
  gradle: 'gradle',
  f: 'fortran',
  f90: 'fortran',
  coffee: 'coffeescript',
  proto: 'protobuf',
  graphql: 'graphql',
  gql: 'graphql',
  sql: 'sql',
  db: 'sqlite',
  sqlite: 'sqlite',
  tex: 'tex',
  org: 'org',
  adoc: 'asciidoc',
  txt: 'text',
  log: 'text',
  csv: 'excel',
  tsv: 'excel',

  // shell
  sh: 'shell',
  bash: 'shell',
  zsh: 'shell',
  fish: 'shell',
  ps1: 'powershell',
  bat: 'bat',
  cmd: 'bat',
  awk: 'awk',

  // documents
  pdf: 'pdf2',
  doc: 'word',
  docx: 'word',
  xls: 'excel',
  xlsx: 'excel',
  ppt: 'powerpoint',
  pptx: 'powerpoint',
  epub: 'word',

  // media / images
  png: 'image',
  jpg: 'image',
  jpeg: 'image',
  gif: 'image',
  webp: 'image',
  bmp: 'image',
  ico: 'image',
  svg: 'svg',
  mp3: 'audio',
  wav: 'audio',
  flac: 'audio',
  ogg: 'audio',
  m4a: 'audio',
  mp4: 'video',
  mov: 'video',
  avi: 'video',
  mkv: 'video',
  webm: 'video',

  // archives / binaries
  zip: 'zip',
  gz: 'zip',
  bz2: 'zip',
  xz: 'zip',
  '7z': 'zip',
  rar: 'zip',
  taz: 'zip',
  ttf: 'font',
  otf: 'font',
  woff: 'font',
  woff2: 'font',
  wasm: 'wasm',
  exe: 'binary',
  dll: 'binary',
  so: 'binary',
  dylib: 'binary',
  bin: 'binary',
  o: 'binary',
  a: 'binary',
}

// Exact filename matches win over extension lookups; keys are lowercase.
const EXACT_NAMES: Record<string, string> = {
  'dockerfile': 'docker',
  'dockerfile.dev': 'docker',
  '.dockerignore': 'docker',
  'makefile': 'config',
  'gnumakefile': 'config',
  'cmakelists.txt': 'cmake',
  'cmakecache.txt': 'cmake',
  'go.mod': 'go',
  'go.sum': 'go',
  'go.work': 'go-work',
  'package.json': 'npm',
  'package-lock.json': 'npm',
  'npm-shrinkwrap.json': 'npm',
  'yarn.lock': 'yarn',
  'pnpm-lock.yaml': 'pnpm',
  'bun.lockb': 'bun',
  'bun.lock': 'bun',
  'cargo.toml': 'rust',
  'cargo.lock': 'rust-toolchain',
  '.gitignore': 'git',
  '.gitattributes': 'git',
  '.gitmodules': 'git',
  '.gitkeep': 'git',
  '.mailmap': 'git',
  '.env': 'config',
  '.env.local': 'config',
  '.env.example': 'config',
  '.editorconfig': 'editorconfig',
  '.eslintrc': 'eslint',
  '.eslintrc.js': 'eslint',
  '.eslintrc.json': 'eslint',
  '.eslintrc.yml': 'eslint',
  '.prettierrc': 'prettier',
  '.prettierrc.json': 'prettier',
  'license': 'license',
  'licence': 'license',
  'license.md': 'license',
  'license.txt': 'license',
  'unlicense': 'license',
  'tsconfig.json': 'tsconfig',
  'jsconfig.json': 'jsconfig',
  'vite.config.ts': 'vite',
  'vite.config.js': 'vite',
  'vitest.config.ts': 'vitest',
  'vitest.config.js': 'vitest',
  'webpack.config.js': 'webpack',
  'webpack.config.ts': 'webpack',
  'rollup.config.js': 'rollup',
  'rollup.config.mjs': 'rollup',
  'jest.config.js': 'jest',
  'jest.config.ts': 'jest',
  'babel.config.js': 'babel',
  '.babelrc': 'babel',
  'postcss.config.js': 'postcss',
  'postcss.config.cjs': 'postcss',
  'tailwind.config.js': 'tailwind',
  'tailwind.config.ts': 'tailwind',
  'next.config.js': 'next',
  'next.config.mjs': 'next',
  'nuxt.config.ts': 'nuxt',
  'nginx.conf': 'nginx',
  '.http': 'apache',
  'robots.txt': 'text',
  'contributing.md': 'markdown',
  'code_of_conduct.md': 'markdown',
}

let collection: Record<string, IconifyIcon> | null = null
let pending: Promise<void> | null = null
