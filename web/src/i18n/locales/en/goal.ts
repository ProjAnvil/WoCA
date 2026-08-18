export default {
  title: 'Goal Mode',
  hint:
    'Set a persistent goal and woca will autonomously run "evaluate → execute → verify" cycles until an independent evaluator marks it complete, it becomes blocked, or the budget is exhausted. You can also type',
  hintObjective: '<objective>',
  hintSuffix: '.',
  objectivePlaceholder:
    'Objective, e.g. add integration tests for the goal API in internal/server and get them all green',
  maxCycles: 'Max cycles',
  tokenBudget: 'Token budget',
  start: 'Start goal loop',
  claimPending: 'Completion claim pending verification',
  running: 'Cycle running…',
  cycles: 'Cycles',
  blocked: 'Blocked: {reason}',
  evidence: 'Evidence: {evidence}',
  claim: 'Executor claim: {evidence}',
  latestNote: 'Latest note: {note}',
  pause: 'Pause',
  resume: 'Resume',
  status: {
    active: 'Active',
    paused: 'Paused',
    blocked: 'Blocked',
    complete: 'Complete',
    budgetLimited: 'Budget exhausted',
  },
}
