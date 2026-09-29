// Human names for every enum value the steering and work APIs return.
export const intents = ["correction", "direction", "clarification", "routine"] as const;
export type Intent = (typeof intents)[number];

export const wentWrongs = [
  "missed_requirement",
  "broke_convention",
  "wrong_approach",
  "unverified_done",
  "wrong_area",
  "over_engineered",
  "lacked_domain_knowledge",
  "environment",
  "other",
] as const;
export type WentWrong = (typeof wentWrongs)[number];

export const preventions = [
  "instruction",
  "skill",
  "tool_access",
  "verification",
  "clearer_ticket",
  "stronger_model",
  "nothing",
] as const;
export type Prevention = (typeof preventions)[number];

export const taskTypes = ["bug", "feature", "refactor", "test", "config", "docs"] as const;

export const groupBys = ["workspace", "repo", "agent", "model", "harness", "taskType"] as const;
export type GroupBy = (typeof groupBys)[number];

export const signals = [
  "follow_up",
  "interruption",
  "denial",
  "rewind",
  "human_edit",
  "abandoned",
  "restarted",
  "human_rewrite",
  "review_change",
  "ci_fix",
  "revert",
  "fix",
] as const;

const intentNames: Record<string, string> = {
  correction: "Correction",
  direction: "Direction",
  clarification: "Clarification",
  routine: "Routine",
};

const wentWrongNames: Record<string, string> = {
  missed_requirement: "Missed a requirement",
  broke_convention: "Broke a convention",
  wrong_approach: "Wrong approach",
  unverified_done: "Claimed done without verifying",
  wrong_area: "Touched the wrong area",
  over_engineered: "Over-engineered",
  lacked_domain_knowledge: "Lacked domain knowledge",
  environment: "Environment or tooling problem",
  other: "Other",
  unclassified: "Unclassified",
};

const preventionNames: Record<string, string> = {
  instruction: "An instruction in AGENTS.md or rules",
  skill: "A skill or procedure",
  tool_access: "Tool or MCP access",
  verification: "A verification step",
  clearer_ticket: "A clearer ticket",
  stronger_model: "A stronger model",
  nothing: "Nothing in the harness",
};

// Verb phrases for the prevention mix sentence: "35% of corrections trace to unclear tickets".
const preventionPhrases: Record<string, string> = {
  instruction: "need an instruction in AGENTS.md or rules",
  skill: "need a skill or procedure",
  tool_access: "need tool or MCP access",
  verification: "need a verification step",
  clearer_ticket: "trace to unclear tickets",
  stronger_model: "need a stronger model",
  nothing: "have no fix in the harness",
};

const signalNames: Record<string, string> = {
  follow_up: "Follow-up message",
  interruption: "Interruption",
  denial: "Denied tool call",
  rewind: "Rewind",
  human_edit: "Human edit",
  abandoned: "Abandoned session",
  restarted: "Restarted with another agent or model",
  human_rewrite: "Human rewrite before merge",
  review_change: "Review comment, then a change",
  ci_fix: "Human fix for failed CI",
  revert: "Revert",
  fix: "Fix after merge",
};

const phaseNames: Record<string, string> = {
  in_session: "In session",
  before_merge: "Before merge",
  after_merge: "After merge",
};

const labelSourceNames: Record<string, string> = {
  model: "Model",
  rule: "Rule",
  human: "Relabeled by a person",
};

const taskTypeNames: Record<string, string> = {
  bug: "Bug",
  feature: "Feature",
  refactor: "Refactor",
  test: "Test",
  config: "Config",
  docs: "Docs",
};

const groupByNames: Record<string, string> = {
  workspace: "Workspace",
  repo: "Repository",
  agent: "Agent",
  model: "Model",
  harness: "Harness version",
  taskType: "Task type",
};

const agentNames: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
  "cursor-cli": "Cursor CLI",
  cursor: "Cursor",
};

const sourceNames: Record<string, string> = {
  import: "imported logs",
  hook: "hooks",
  otel: "OpenTelemetry",
  entire: "Entire",
};

const promptModeNames: Record<string, string> = {
  off: "Off: no prompt text, structural signals only",
  redacted: "Redacted: prompt text with secrets removed",
  full: "Full: prompt text and tool output",
};

const linkSourceNames: Record<string, string> = {
  explicit: "linked by a person",
  branch: "branch name",
  pull_request: "pull request",
  same_branch: "same branch as a linked pull request",
  same_work_item: "same work item",
};

const snapshotReasonNames: Record<string, string> = {
  first_linked: "first linked",
  work_started: "work started",
  text_changed: "summary or description changed",
  resolved: "resolved",
};

const roleNames: Record<string, string> = { viewer: "Viewer", member: "Member", admin: "Admin", owner: "Owner" };

// Cases (docs/specs/cases.md).
export const caseKinds = ["capability", "regression", "steering"] as const;
export const caseStatuses = ["mined", "validated", "validation_failed", "approved", "rejected", "retired"] as const;
export const caseSplits = ["dev", "held_out"] as const;
export const assertionKinds = ["forbidden_file", "command_before_done", "diff_must_match", "diff_must_not_match"] as const;
export type AssertionKind = (typeof assertionKinds)[number];

const caseKindNames: Record<string, string> = {
  capability: "Capability",
  regression: "Regression",
  steering: "Steering",
};

const caseStatusNames: Record<string, string> = {
  mined: "Mined, not validated",
  validated: "Validated",
  validation_failed: "Validation failed",
  approved: "Approved",
  rejected: "Rejected",
  retired: "Retired",
};

const caseScopeNames: Record<string, string> = {
  single: "Single repository",
  split: "Split: one repository sealed, the rest as context",
  multi: "Multi-repository",
};

// For table cells, where the full scope name does not fit.
const caseScopeShortNames: Record<string, string> = { single: "Single", split: "Split", multi: "Multi-repo" };

const caseSplitNames: Record<string, string> = {
  dev: "Dev set",
  held_out: "Held-out set",
};

const repoRoleNames: Record<string, string> = {
  sealed: "Sealed: the agent works on it",
  context: "Context: present at its merged state",
};

const validationFailureNames: Record<string, string> = {
  build: "The environment does not build at the base commit",
  no_fail_to_pass: "No test fails at the base and passes with the change",
  fail_to_pass_passed_at_base: "A fail-to-pass test already passes at the base commit",
  flaky: "A test did not pass 3 runs out of 3",
  too_slow: "The slowest test run takes 10 minutes or more",
  fix_does_not_fail: "The original change alone passes the fix's new tests",
  apply_failed: "The change does not apply to the base commit",
};

const retireReasonNames: Record<string, string> = {
  environment: "The environment no longer builds",
  drift: "Harness drift passed one half of the harness references",
  manual: "Retired by an Admin",
};

const assertionKindNames: Record<string, string> = {
  forbidden_file: "Must not change files matching",
  command_before_done: "Must run a command before saying done",
  diff_must_match: "The diff must match",
  diff_must_not_match: "The diff must not match",
};

function named(map: Record<string, string>, value: string | null | undefined): string {
  if (value == null) return "—";
  return map[value] ?? value.replaceAll("_", " ");
}

export const intentName = (v?: string | null) => named(intentNames, v);
export const wentWrongName = (v?: string | null, label?: string | null) =>
  v === "other" && label ? `Other: ${label}` : named(wentWrongNames, v);
export const preventionName = (v?: string | null) => named(preventionNames, v);
export const preventionPhrase = (v?: string | null) => named(preventionPhrases, v);
export const signalName = (v?: string | null) => named(signalNames, v);
export const phaseName = (v?: string | null) => named(phaseNames, v);
export const labelSourceName = (v?: string | null) => named(labelSourceNames, v);
export const taskTypeName = (v?: string | null) => named(taskTypeNames, v);
export const groupByName = (v?: string | null) => named(groupByNames, v);
export const agentName = (v?: string | null) => named(agentNames, v);
export const sourceName = (v?: string | null) => named(sourceNames, v);
export const promptModeName = (v?: string | null) => (v == null ? "Not chosen: the server accepts no sessions" : named(promptModeNames, v));
export const linkSourceName = (v?: string | null) => named(linkSourceNames, v);
export const snapshotReasonName = (v?: string | null) => named(snapshotReasonNames, v);
export const roleName = (v?: string | null) => named(roleNames, v);
export const caseKindName = (v?: string | null) => named(caseKindNames, v);
export const caseStatusName = (v?: string | null) => named(caseStatusNames, v);
export const caseScopeName = (v?: string | null) => named(caseScopeNames, v);
export const caseScopeShortName = (v?: string | null) => named(caseScopeShortNames, v);
export const caseSplitName = (v?: string | null) => named(caseSplitNames, v);
export const repoRoleName = (v?: string | null) => named(repoRoleNames, v);
export const validationFailureName = (v?: string | null) => named(validationFailureNames, v);
export const retireReasonName = (v?: string | null) => named(retireReasonNames, v);
export const assertionKindName = (v?: string | null) => named(assertionKindNames, v);

// SDD 6: instruction, skill and verification are in the harness; the rest are outside it.
export const harnessFixable = (p: string) => p === "instruction" || p === "skill" || p === "verification";
