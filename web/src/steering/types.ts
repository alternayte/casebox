import type { Rate } from "@/lib/format";
import type { GroupBy } from "@/lib/labels";

// The contract in docs/specs/steering.md.
export type Quantiles = { median: number; p75: number };

export type Report = {
  period: { from: string; to: string };
  k: number;
  coverage: {
    sessions: number;
    people: number;
    unmappedSessions: number;
    agents: { agent: string; sessions: number; sources: string[] }[];
    promptMode: string | null;
    interventions: number;
    pending: number;
    unclassified: number;
    workerSeen: boolean;
  };
  headline: {
    correctionFreeRate: Rate | null;
    correctionsPerWorkItem: { inSession: Quantiles | null; beforeMerge: Quantiles | null; afterMerge: Quantiles | null; n: number } | null;
    // turns and toolCalls are null when no run in the sample had a correction.
    autonomousRun: { turns: Quantiles | null; toolCalls: Quantiles | null; n: number; uncorrected: number } | null;
    afterMergeRate: Rate | null;
    abandonmentRate: Rate | null;
  };
  themes: Theme[];
  preventionMix: { prevention: string; share: number; corrections: number; harnessFixable: boolean }[];
  afterMerge: { reverts: number; fixes: number; people: number } | null;
  groups: Group[];
  hidden: Record<string, number>;
};

export type Theme = {
  wentWrong: string;
  label: string | null;
  corrections: number;
  people: number;
  harnessFixable: boolean;
  prevention: { prevention: string; share: number }[];
  phases: { inSession: number; beforeMerge: number; afterMerge: number };
  quotes: { ref: string; text: string; day: string }[];
  // The patterns of this what-went-wrong class, each with at least k people behind it.
  patterns?: { id: string; title: string; status: string; advisory: boolean }[] | null;
};

export type Group = {
  key: string;
  people: number;
  sessions: number;
  correctionFreeRate: Rate | null;
  abandonmentRate: Rate | null;
  afterMergeRate: Rate | null;
  corrections: number;
};

export type Intervention = {
  ref: string;
  signal: string;
  phase: string;
  day: string;
  text: string | null;
  intent: string | null;
  wentWrong: string | null;
  wentWrongLabel: string | null;
  prevention: string | null;
  labelSource: "model" | "rule" | "human" | null;
  confidence: number | null;
};

export type InterventionPage = { interventions: Intervention[]; page: number; pageSize: number; total: number };

export type Relabel = { ref: string; intent: string; wentWrong?: string; wentWrongLabel?: string; prevention?: string };

export type Status = { sessions: number; interventions: number; pending: number; unclassified: number; workerSeen: boolean; analysisWorkers: number };

// The report filters, as kept in the URL.
export type Filters = {
  from?: string;
  to?: string;
  workspace?: string;
  repo?: string;
  agent?: string;
  model?: string;
  harness?: string;
  taskType?: string;
  groupBy?: GroupBy;
};

export const filterKeys = ["from", "to", "workspace", "repo", "agent", "model", "harness", "taskType", "groupBy"] as const;
