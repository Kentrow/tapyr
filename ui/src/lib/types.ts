// Types of the status API (/api/v1), mirroring internal/api/types.go.

export interface List<T> {
  items: T[];
  next_cursor?: string;
}

export interface Reload {
  at: string;
  success: boolean;
  error?: string;
}

export interface Status {
  version: string;
  revision: string;
  go_version: string;
  started_at: string;
  uptime_seconds: number;
  config_hash: string;
  last_reload: Reload;
  ready: boolean;
  readiness_reasons: string[];
  plan_feasible: boolean;
  instances: number;
}

export type Health = "ok" | "degraded" | "failing" | "unknown";

export interface Provider {
  name: string;
  version: string;
  description?: string;
  accounts: number;
  jobs: number;
  health: Health;
}

export interface Target {
  name: string;
  labels: Record<string, string>;
  circuit: string;
}

export interface Account {
  name: string;
  provider: string;
  labels: Record<string, string>;
  targets: Target[];
  circuit?: string;
  auth: { type: string; token_expiry?: string; reauth_required: boolean };
  health: Health;
  // Why the account is not ok (redacted job errors, a re-bootstrap required,
  // open circuits); empty when ok.
  health_reasons: string[];
  // The first thing to fix when the account is not ok.
  problem?: Problem;
}

export interface RunSummary {
  run_id: string;
  instance: string;
  result: string;
  started_at: string;
  ended_at: string;
  error?: string;
  // The error in words, with what to do, when the run did not succeed.
  problem?: Problem;
}

// Problem says what went wrong in words and what to do (spec 09,
// "Messages"); the technical error is shown next to it.
export interface Problem {
  class: string;
  title: string;
  action: string;
  // When it should end on its own (a quota reset, the end of a pause).
  until?: string;
  // The section of the troubleshooting guide of the running version.
  docs_url: string;
}

export interface Job {
  name: string;
  provider: string;
  account: string;
  template?: string;
  role: string;
  priority: string;
  implicit: boolean;
  // What an implicit job lists ("networks") and the configured jobs it
  // lists them for.
  items?: string;
  discovery_for?: string[];
  parent?: string;
  desired_interval_seconds: number;
  effective_interval_seconds: number;
  stretch_reason?: string;
  instances: number;
  // False for a job whose metrics no output keeps, or a parent whose
  // inventory no collected job takes (ADR 0013): neither scheduled nor planned.
  collected: boolean;
  not_collected_reason?: string;
  outputs: string[];
  labels: Record<string, string>;
  last_run?: RunSummary;
  next_run?: string;
  // What a collected job that has not run yet waits for; absent once it
  // has run.
  waiting?: Waiting;
  definition?: unknown;
}

export type WaitingReason = "first_run" | "parent" | "parent_failing";

export interface Waiting {
  // first_run: its first run is due at until; parent: a fan-out whose
  // parent has not listed its items yet and runs next at until;
  // parent_failing: the parent, or one of its own parents (failing_job),
  // fails.
  reason: WaitingReason;
  until?: string;
  parent?: string;
  failing_job?: string;
  error?: string;
  failed_at?: string;
  problem?: Problem;
}

export interface Instance {
  key: string;
  target?: string;
  vars: Record<string, string>;
  last_success?: string;
  series: number;
  next_run?: string;
  last_run?: RunSummary;
}

export interface Run extends RunSummary {
  duration_seconds: number;
  requests: number;
  retries: number;
  pages: number;
  items: number;
  samples: number;
}

export interface Budget {
  provider: string;
  limit: string;
  kind: "quota" | "rate" | "concurrency";
  scope: string;
  // The accounts whose requests can count in the scope.
  accounts: string[];
  max?: number;
  usable?: number;
  reserve?: number;
  used?: number;
  remaining?: number;
  remaining_observed?: number;
  external_usage?: number;
  reserve_remaining?: number;
  window_start?: string;
  window_end?: string;
  rolling?: boolean;
  exhaustion_eta_seconds?: number;
  limit_mismatch?: boolean;
  rate_per_second?: number;
  burst?: number;
  tokens?: number;
  in_use?: number;
  penalty_until?: string;
  // What the provider definition says of the limit.
  description?: string;
  source?: { kind: "vendor-docs" | "measured" | "assumed"; url?: string; date?: string };
  // The calendar window of a quota (minute ... month), absent when rolling.
  period?: string;
}

export interface Bucket {
  start: string;
  count: number;
}

export interface TimelineWindow {
  start: string;
  end: string;
  buckets: Bucket[];
}

export interface Timeline {
  provider: string;
  limit: string;
  scope: string;
  bucket_seconds: number;
  current: TimelineWindow;
  previous?: TimelineWindow;
}

export interface PlanJob {
  name: string;
  provider: string;
  account: string;
  priority: string;
  desired_interval_seconds: number;
  effective_interval_seconds: number;
  stretch: number;
  reason?: string;
  instances: number;
  pages_per_run: number;
}

export interface PlanScope {
  limit: string;
  scope: string;
  period_seconds: number;
  budget: number;
  reserve: number;
  projected: number;
  feasible: boolean;
  exhaustion_at?: string;
  jobs: string[];
  // Why the scope does not fit and what to change, when it does not.
  problem?: Problem;
}

export interface Plan {
  at: string;
  feasible: boolean;
  jobs: PlanJob[];
  scopes: PlanScope[];
}

export interface GraphNode {
  id: string;
  kind: string;
  label: string;
  health: Health;
  instances?: number;
  // The waiting reason of a job node that has not run yet.
  waiting?: WaitingReason;
}

export interface Graph {
  nodes: GraphNode[];
  edges: { from: string; to: string }[];
}

export interface Output {
  name: string;
  type: string;
  queue: number;
  capacity: number;
  sent: number;
  dropped: number;
  last_success?: string;
  last_error?: string;
  last_error_at?: string;
  // The series the output receives, after its processing rules and its
  // limit (ADR 0014); max_series is absent without a limit.
  series: number;
  max_series?: number;
  // New series held back by the limit at the last runs, and samples it
  // dropped since the start.
  series_refused: number;
  samples_refused: number;
  // Jobs sent to the output without a successful run yet: not counted.
  unmeasured_jobs: string[];
}

export interface DebugPage {
  status: number;
  headers: Record<string, string>;
  body: string;
  truncated: boolean;
}

export interface DebugSample {
  name: string;
  type: string;
  labels: Record<string, string>;
  value: number;
}

export interface Debug {
  job: string;
  instance: string;
  run_id: string;
  time: string;
  result: string;
  error?: string;
  problem?: Problem;
  pages: DebugPage[];
  samples: DebugSample[];
  instances: string[];
}

export interface ApiError {
  error: { code: string; message: string };
}
