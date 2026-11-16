// Pure helpers for the human-in-the-loop card stack (design §4): answer-key
// derivation, ask extraction off the mixed legacy/envelope payloads, risk
// tiers, and the answer-value builders for the {answers:[{key,value}]} body.
import type { ApprovalAsk, CardAnswerEntry, InterruptCardData } from '../types'

/** The answer-map key addressing one parked card (its NS, else its ID). */
export function hitlCardKey(card: InterruptCardData): string {
  return card.ns || card.id
}

/**
 * The asks of a card, whatever shape it arrived in: approvals cards carry
 * the whole envelope as payload; single-ask legacy cards embed `asks` in
 * the flat payload (Task 8's forward-compat field). Legacy cards without
 * asks return [] — they answer with the flat approve flag only.
 */
export function asksOf(card: InterruptCardData): ApprovalAsk[] {
  const p = card.payload
  if (typeof p !== 'object' || p === null) return []
  const asks = (p as { asks?: unknown }).asks
  if (card.kind === 'approvals') {
    return Array.isArray((p as { asks?: unknown }).asks) ? ((p as { asks: ApprovalAsk[] }).asks) : []
  }
  return Array.isArray(asks) ? (asks as ApprovalAsk[]) : []
}

/** The display identity of one card: the ask's call id when the card is a
 * single ask, else the card's own id (falling back to its answer key).
 * Used for testid suffixes and the short code. */
export function hitlRequestId(card: InterruptCardData): string {
  const asks = asksOf(card)
  if (asks.length === 1 && asks[0].call_id) return asks[0].call_id
  return card.id || hitlCardKey(card)
}

/** Who raised the card: the ask's origin, a legacy subagent name, or main. */
export function hitlOrigin(card: InterruptCardData): string {
  const asks = asksOf(card)
  if (asks.length && asks[0].origin) return asks[0].origin
  const p = card.payload
  if (typeof p === 'object' && p !== null) {
    const sub = (p as { subagent?: unknown }).subagent
    if (typeof sub === 'string' && sub) return sub
  }
  return 'main'
}

/** The 4-char call-id short code shown next to the origin badge. */
export function hitlShortCode(card: InterruptCardData): string {
  const asks = asksOf(card)
  const src = (asks.length === 1 && asks[0].call_id) || card.id || card.ns
  return src.slice(-4)
}

/** Risk rail tiers (§4.2): read_only→neutral, write→warn, network/destructive→danger. */
export type RiskTier = 'neutral' | 'warn' | 'danger'

const NETWORK_TOOLS = new Set(['WebFetch', 'WebSearch', 'BrowserNavigate', 'BrowserClick', 'BrowserType'])
const DESTRUCTIVE_WORDS = ['rm -rf', 'mkfs', 'dd if=', 'git push --force', 'git push -f', 'drop table', 'drop database', 'truncate table', 'shred ']

/** Mirrors the server's classification closely enough for the color cue
 * (the gate itself always re-evaluates server-side). */
export function riskTier(toolName: string, args: Record<string, unknown> | undefined, isReadOnly: boolean): RiskTier {
  const command = typeof args?.command === 'string' ? args.command : ''
  if (NETWORK_TOOLS.has(toolName)) return 'danger'
  if (command && DESTRUCTIVE_WORDS.some((w) => command.includes(w))) return 'danger'
  if (isReadOnly) return 'neutral'
  return 'warn'
}

/** The rule key an "allow for session" click would create for this card
 * ("" when the card's asks carry no grantable rule). */
export function sessionRuleKeyOf(card: InterruptCardData): string {
  const asks = asksOf(card)
  if (!asks.length) return ''
  for (const a of asks) {
    if (a.session_rule_key) return a.session_rule_key
  }
  return ''
}

/** One ask-level decision of an approvals answer. */
export interface AskDecisionValue {
  outcome: 'allow' | 'deny'
  grant?: 'session'
  answers?: string[]
}

/** Builds the answer entry for one card from an ask-level choice. */
export function cardAnswer(card: InterruptCardData, outcome: 'allow' | 'deny', grant?: 'session'): CardAnswerEntry {
  const asks = asksOf(card)
  if (!asks.length) {
    // Legacy flat card (permission/plan): the approve flag only.
    return { key: hitlCardKey(card), value: { approve: outcome === 'allow' } }
  }
  const decisions: Record<string, AskDecisionValue> = {}
  for (const a of asks) {
    decisions[a.call_id] = grant === 'session' ? { outcome, grant } : { outcome }
  }
  return { key: hitlCardKey(card), value: { decisions } }
}

/** Builds the answer entry for an ask (AskUserQuestion) card. */
export function askCardAnswer(card: InterruptCardData, answers: string[]): CardAnswerEntry {
  return { key: hitlCardKey(card), value: { answers } }
}
