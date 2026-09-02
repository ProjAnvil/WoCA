---
name: executing-plans
description: Use when you have a written implementation plan to execute in a separate session with review checkpoints
---

> Vendored from [obra/superpowers](https://github.com/obra/superpowers) @ `b36e082` (MIT, © 2025 Jesse Vincent) and adapted for woca: skill cross-references are de-namespaced, tool names map to woca's equivalents, and the companion reference files are inlined below.

# Executing Plans

## Overview

Load plan, review critically, execute all tasks, report when complete.

**Announce at start:** "I'm using the executing-plans skill to implement this plan."

**Note:** This skill works much better with subagents — tell your human partner. When the Task tool is available, prefer dispatching a fresh subagent per task with review between tasks instead of executing everything inline.

## The Process

### Step 1: Load and Review Plan
1. Ensure an isolated workspace: use woca's EnterWorktree tool to create one or verify the existing one
2. Read plan file
3. Review critically - identify any questions or concerns about the plan
4. If concerns: Raise them with your human partner before starting
5. If no concerns: Create todos for the plan items and proceed

### Step 2: Execute Tasks

For each task:
1. Mark as in_progress
2. Follow each step exactly (plan has bite-sized steps)
3. Run verifications as specified
4. Mark as completed

### Step 3: Complete Development

After all tasks complete and verified:
- Verify the full test suite passes (see verification-before-completion)
- Summarize the change for your human partner and present completion options (merge the branch, open a PR, or keep it for review)

## When to Stop and Ask for Help

**STOP executing immediately when:**
- Hit a blocker (missing dependency, test fails, instruction unclear)
- Plan has critical gaps preventing starting
- You don't understand an instruction
- Verification fails repeatedly

**Ask for clarification rather than guessing.**

## When to Revisit Earlier Steps

**Return to Review (Step 1) when:**
- Partner updates the plan based on your feedback
- Fundamental approach needs rethinking

**Don't force through blockers** - stop and ask.

## Remember
- Review plan critically first
- Follow plan steps exactly
- Don't skip verifications
- Reference skills when plan says to
- Stop when blocked, don't guess
- Never start implementation on main/master branch without explicit user consent
