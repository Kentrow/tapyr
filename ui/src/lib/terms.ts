// terms are the help texts of the columns and figures of the web UI: the
// definitions of the glossary (docs/glossary.md) in a sentence or two.
// Keep both in step.
export const terms = {
  quota: "A number of calls the provider allows per window (a day, a month, a rolling hour). Once it is spent, Tapyr makes no more calls in its scope until the reset.",
  scope: "What the quota is counted per: an account, an organization, a credential shared by several accounts, or the whole Tapyr process.",
  untilReset: "The time left before the window of the quota starts again.",
  available: "What the quota leaves to Tapyr until its reset: its maximum, times the share of the account, minus what is already spent and the calls kept for retries.",
  projected: "What the jobs will spend until the reset at their planned intervals.",
  reserve: "Calls kept for retries and critical jobs: the planner leaves them out, so that a retry never finds the quota empty.",
  use: "The projected spend against what is available.",
  priority: "Who slows down first when a quota is tight: low, then normal, then high. Critical jobs are never slowed down.",
  role: "metrics: the job produces samples. inventory: it lists items for other jobs. both: it does both.",
  desired: "How often the configuration asks the job to run.",
  effective: "How often the job actually runs: its desired interval, or longer when the planner slows it down to fit a quota.",
  stretch: "How much the planner slowed the job down to fit a quota: x2 runs it half as often. The jobs of a priority slow down together, up to 12 times, before the next priority up.",
  instances: "The copies of the job that run: one per target, and for a fan-out one per item its parent listed (a network, a device).",
  pages: "The requests a run makes when the answer comes in several pages: the median of the last runs, else the estimate of the template.",
} as const;
