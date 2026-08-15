// Coarse client-side mirror of the server's previewable-extension list so
// the tree knows which files are clickable; the server makes the final call.
const BINARY_EXTS = new Set([
  'png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svgz', 'tif', 'tiff',
  'mp3', 'mp4', 'wav', 'flac', 'ogg', 'webm', 'mov', 'avi', 'mkv',
  'pdf', 'zip', 'gz', 'tar', 'tgz', 'bz2', 'xz', '7z', 'rar',
  'exe', 'dll', 'so', 'dylib', 'bin', 'o', 'a', 'class', 'jar', 'wasm',
  'woff', 'woff2', 'ttf', 'otf', 'eot', 'db', 'sqlite', 'sqlite3',
  'psd', 'ai', 'sketch', 'dsstore',
])

const DOTFILE_NAMES = new Set([
  '.gitignore', '.gitattributes', '.env', '.editorconfig', '.npmrc', '.nvmrc',
])

/** Whether a workspace file can be opened in the preview pane. */
export function isTextPreviewable(name: string): boolean {
  const lower = name.toLowerCase()
  if (DOTFILE_NAMES.has(lower)) return true
  if (lower.startsWith('.') && !lower.slice(1).includes('.')) return false
  const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : ''
  if (!ext) return true // Makefile, LICENSE, Dockerfile …
  return !BINARY_EXTS.has(ext)
}
