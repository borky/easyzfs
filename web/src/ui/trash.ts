// isTrash — mirrors actions.InTrash: <pool>/easyzfs-trash and anything under
// it (datasets and their snapshots), which the views show as the recycle bin
// rather than as data.
export function isTrash(name: string): boolean {
  return /^[^/@]+\/easyzfs-trash([/@]|$)/.test(name);
}
