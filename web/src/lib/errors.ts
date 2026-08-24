// Error rendering: every catch site in the app surfaces the same shape —
// an Error's message, or String() for anything thrown across a boundary
// (fetch failures, JSON parse errors, non-Error rejections). One helper
// keeps the choice uniform instead of 30 hand-rolled ternaries.
export function toErrorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}
