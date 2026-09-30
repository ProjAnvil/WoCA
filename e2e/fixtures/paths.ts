import path from 'node:path';

export const repoRoot = path.resolve(__dirname, '..', '..');
export const runtimeDir = path.join(repoRoot, 'e2e', '.runtime');
export const labFactsPath = path.join(runtimeDir, 'lab.json');
