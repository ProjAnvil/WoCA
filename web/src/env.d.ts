/// <reference types="vite/client" />

// The vscode-icons collection is a ~3.7MB JSON; letting TypeScript infer
// its full literal shape would explode type-check memory. Only the shape
// we consume is declared here (Vite still inlines the real data at build).
declare module '@iconify-json/vscode-icons/icons.json' {
  const icons: { icons: Record<string, { body: string }> }
  export default icons
}
