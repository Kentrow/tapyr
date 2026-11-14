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
}

export interface RunSummary {
  run_id: string;
  instance: string;
  result: string;
  started_at: string;
  ended_at: string;
  error?: string;
}

export interface Job {
  name: string;
  provider: string;
  account: string;
  template?: string;
  role: string;
  priority: string;
  implicit: boolean;
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
  definition?: unknown;
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
  pages: DebugPage[];
  samples: DebugSample[];
  instances: string[];
}

export interface ApiError {
  error: { code: string; message: string };
}
