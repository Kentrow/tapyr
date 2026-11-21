import { useEffect, useState } from "react";
import { Badge, Failure, Loading, Meter, PageHead, Since } from "../components/ui";
import { Icon } from "../components/Icon";
import { ProblemNote } from "../components/Problem";
import { JobName } from "../components/JobName";
import { count, duration, jobLabels, jobWords, outputHealth, percent, plural, scopeAccounts, seriesStatus, since, waitingText } from "../lib/format";
import { useJSON, useList, useLive, useNow, type LiveEvent } from "../lib/live";
import { href } from "../lib/router";
import type { Account, Budget, Job, Output, Plan, Problem, Provider, Status } from "../lib/types";

type JobState = "ok" | "bad" | "idle";

// A job without a run waits (idle): for its first run, or for its parent.
// Waiting is not failing.
const jobState = (j: Job): JobState => (!j.last_run ? "idle" : j.last_run.result === "success" ? "ok" : "bad");

// jobCounts says how the jobs stand, in words: how many succeeded, failed
// and wait, and what most of the waiting ones wait for.
export function jobCounts(jobs: Job[]): string {
  const ok = jobs.filter((j) => jobState(j) === "ok").length;
  const bad = jobs.filter((j) => jobState(j) === "bad").length;
  const waiting = jobs.filter((j) => jobState(j) === "idle");
  const parts = [`${ok} succeeded`];
  if (bad) parts.push(`${bad} failing`);
  if (waiting.length) {
    const first = waiting.filter((j) => j.waiting?.reason === "first_run").length;
    const failing = waiting.filter((j) => j.waiting?.reason === "parent_failing").length;
    const one = waiting.length === 1;
    const why = first === waiting.length ? (one ? " for its first run" : " for their first run") : failing === waiting.length ? (one ? " for a failing parent" : " for failing parents") : "";
    parts.push(`${waiting.length} waiting${why}`);
  }
  return `${plural(jobs.length, "job")}: ${parts.join(", ")}`;
}
const leftOf = (b: Budget) => Math.max(0, (b.max ?? 0) - (b.used ?? 0) - (b.external_usage ?? 0));

// Overview is the cockpit: four indicators, the conditions that need
// someone, the providers with their jobs and quotas, the live activity and
// the outputs.
export function Overview() {
  const status = useJSON<Status>("/status");
  const providers = useList<Provider>("/providers");
  const accounts = useList<Account>("/accounts");
  const budgets = useList<Budget>("/budgets");
  const jobs = useList<Job>("/jobs");
  const outputs = useList<Output>("/outputs");
  // The plan exists after the first planning; the Overview does without.
  const plan = useJSON<Plan>("/plan");
  const activity = useActivity(jobs.data);
  // The times of the page ("resets in 2h", "up 3h") move with the clock.
  const now = useNow();
  const label = jobLabels(jobs.data);
  if (status.error && !status.data) return <Failure error={status.error} />;
  if (!status.data || !providers.data || !accounts.data || !budgets.data || !jobs.data || !outputs.data) return <Loading />;
  const s = status.data;
  const quotas = budgets.data.filter((b) => b.kind === "quota");
  const tightest = [...quotas].sort((a, b) => leftOf(a) / (a.max || 1) - leftOf(b) / (b.max || 1))[0];
  const okProviders = providers.data.filter((p) => p.health === "ok").length;
  const collected = jobs.data.filter((j) => j.collected !== false);
  const notCollected = jobs.data.length - collected.length;
  const okJobs = collected.filter((j) => jobState(j) === "ok").length;
  const failing = collected.filter((j) => jobState(j) === "bad");
  const waiting = collected.filter((j) => jobState(j) === "idle").length;
  const reauth = accounts.data.filter((a) => a.auth.reauth_required);
  const unhealthy = unhealthyAccounts(accounts.data);
  const limited = outputs.data.filter((o) => o.series_refused > 0);
  const unfit = s.plan_feasible ? [] : (plan.data?.scopes ?? []).filter((sc) => !sc.feasible && sc.problem);
  return (
    <>
      <PageHead title="Overview" sub={<>Tapyr {s.version}, up {duration(uptime(s, now))}, <span title={`Configuration ${s.config_hash.slice(0, 12)} (SHA-256 of the configuration files)`}>configuration loaded {since(s.last_reload.at, now)}</span>.</>} />
      <div className="kpis">
        <Kpi icon="graph" label="Providers healthy" value={`${okProviders}/${providers.data.length}`} ok={okProviders === providers.data.length}
          hint={plural(accounts.data.length, "account")} href={href(["graph"])} />
        <Kpi icon="jobs" label="Jobs succeeding" value={`${okJobs}/${collected.length}`} ok={failing.length === 0}
          hint={(waiting ? `${waiting} waiting to run, ` : "") + plural(s.instances, "instance") + (notCollected ? `, ${notCollected} not collected` : "")} href={href(["jobs"])} />
        <Kpi icon="budgets" label="Tightest quota" value={tightest ? percent(leftOf(tightest), tightest.max ?? 0) + " left" : "No quota"}
          ok={!tightest || tightest.exhaustion_eta_seconds === undefined}
          hint={tightest ? quotaHint(tightest, now) : "rate and concurrency limits only"} href={href(["budgets"])} />
        <Kpi icon="plan" label="Plan" value={s.plan_feasible ? "Fits" : "Does not fit"} ok={s.plan_feasible}
          hint={planHint(plan.data, s.plan_feasible, now)} href={href(["plan"])} />
      </div>

      {(!s.ready || !s.last_reload.success || reauth.length > 0 || unhealthy.length > 0 || limited.length > 0 || unfit.length > 0) && (
        <div className="cells attention">
          {!s.ready && (
            <div className="cell warn">
              <span className="label">Readiness</span>
              <span className="value"><Icon name="alert" />Not ready</span>
              <span className="hint">{s.readiness_reasons.join(", ")}</span>
            </div>
          )}
          {!s.last_reload.success && (
            <div className="cell bad">
              <span className="label">Configuration</span>
              <span className="value"><Icon name="alert" />Reload rejected</span>
              <span className="hint">{since(s.last_reload.at, now)}: {s.last_reload.error}. The previous configuration keeps running.</span>
            </div>
          )}
          {reauth.map((a) => (
            <div className="cell bad" key={a.name}>
              <span className="label">{a.provider}</span>
              <span className="value"><Icon name="alert" />Re-bootstrap required</span>
              {a.problem ? <span className="hint"><ProblemNote problem={a.problem} /></span>
                : <span className="hint">Account {a.name}: its credentials can no longer be renewed. Generate new ones and reload.</span>}
            </div>
          ))}
          {unfit.map((sc) => (
            <div className="cell bad" key={`plan-${sc.limit}-${sc.scope}`}>
              <span className="label"><a href={href(["plan"])}>Plan</a>, {sc.limit}</span>
              <span className="value"><Icon name="alert" />Plan does not fit</span>
              <span className="hint"><ProblemNote problem={sc.problem} /></span>
            </div>
          ))}
          {limited.map((o) => (
            <a className="cell warn" key={`limit-${o.name}`} href={href(["outputs"])}>
              <span className="label">Output {o.name}</span>
              <span className="value"><Icon name="alert" />Series limit reached</span>
              <span className="hint">{seriesStatus(o).detail}</span>
            </a>
          ))}
          {unhealthy.map((a) => (
            <div className={`cell ${a.health === "failing" ? "bad" : "warn"}`} key={`health-${a.name}`}>
              <span className="label">{a.provider} / {a.name}</span>
              <span className="value"><Icon name="alert" />Account {a.health}</span>
              <span className="hint"><ProblemNote problem={a.problem} detail={a.reasons.join("; ")} /></span>
            </div>
          ))}
        </div>
      )}

      <div className="cockpit">
        <div className="stack">
          <h2>Providers</h2>
          {providers.data.map((p) => (
            <ProviderCard key={p.name} provider={p} accounts={accounts.data!.filter((a) => a.provider === p.name)}
              jobs={collected.filter((j) => j.provider === p.name)} quotas={quotas.filter((q) => q.provider === p.name)} />
          ))}
        </div>
        <div className="stack">
          <div className="card activity">
            <div className="row" style={{ justifyContent: "space-between" }}>
              <h2 style={{ margin: 0 }}>Activity</h2>
              <span className="muted small">latest first</span>
            </div>
            <ol>
              {activity.map((a) => (
                <li key={a.key} className={a.ok ? "ok" : "bad"}>
                  <span className="dot" aria-hidden="true" />
                  <div>
                    <strong>{label(a.title)}</strong>
                    <span className="muted small"> {since(a.at, now)}</span>
                    <div className="small muted">{a.detail}</div>
                  </div>
                </li>
              ))}
              {activity.length === 0 && <li className="muted small">No run yet.</li>}
            </ol>
          </div>
          {failing.length > 0 && (
            <div className="card">
              <h2 style={{ margin: 0 }}>Recent errors</h2>
              <ol className="errors">
                {failing.map((j) => (
                  <li key={j.name}>
                    <JobName strong compact name={j.name} job={j} />
                    <span className="muted small"> {since(j.last_run?.ended_at, now)}</span>
                    <div className="small"><ProblemNote problem={j.last_run?.problem} detail={j.last_run?.error} /></div>
                  </li>
                ))}
              </ol>
            </div>
          )}
        </div>
      </div>

      <section className="section">
        <h2>Outputs</h2>
        <div className="row">
          {outputs.data.map((o) => {
            const bad = outputHealth(o) === "failing";
            const limited = o.series_refused > 0;
            return (
              <a key={o.name} className={`chip ${bad ? "bad" : limited ? "warn" : "ok"}`} href={href(["outputs"])}
                title={limited ? seriesStatus(o).detail : undefined}>
                <Icon name="outputs" /> {o.name}
                <span className="muted">
                  {bad ? "failing" : o.type === "prometheus" ? "/metrics" : `${count(o.sent)} sent`}, {plural(o.series, "series", "series")}
                  {limited ? ", series limit reached" : ""}
                </span>
              </a>
            );
          })}
        </div>
      </section>
    </>
  );
}

// unhealthyAccounts are the accounts that need a look, failing first, with
// their reasons. A re-bootstrap has its own cell: the reasons that name it
// are left out, and the account too when no other reason is left.
export function unhealthyAccounts(accounts: Account[]): { name: string; provider: string; health: Account["health"]; reasons: string[]; problem?: Problem }[] {
  const rank = (h: string) => (h === "failing" ? 0 : 1);
  return accounts
    .filter((a) => a.health === "failing" || a.health === "degraded")
    .map((a) => ({
      name: a.name,
      provider: a.provider,
      health: a.health,
      reasons: a.auth.reauth_required ? (a.health_reasons ?? []).filter((r) => !r.includes("re-bootstrap")) : a.health_reasons ?? [],
      // The re-bootstrap has its own cell, with its problem.
      problem: a.auth.reauth_required ? undefined : a.problem,
    }))
    .filter((a) => a.reasons.length > 0)
    .sort((a, b) => rank(a.health) - rank(b.health) || a.name.localeCompare(b.name));
}

// uptime is how long Tapyr has run, from its start time, so that it moves
// with the clock between two loads of the status.
export function uptime(s: Pick<Status, "started_at" | "uptime_seconds">, now = Date.now()): number {
  const started = Date.parse(s.started_at);
  return Number.isNaN(started) ? s.uptime_seconds : Math.max(s.uptime_seconds, (now - started) / 1000);
}

// quotaHint names a quota and when it resets, and, when it would run out
// before then at the current pace, when it runs out: the red state of the
// indicator then carries words.
export function quotaHint(b: Budget, now = Date.now()): string {
  const name = `${b.provider} / ${b.limit}`;
  if (b.exhaustion_eta_seconds !== undefined) return `${name}, runs out in ${duration(b.exhaustion_eta_seconds)} at the current pace, resets ${since(b.window_end, now)}`;
  return `${name}, resets ${since(b.window_end, now)}`;
}

// planHint says what the plan means: every critical job on schedule, or
// which quota would run out, and when, with the planned intervals (the
// projection of the plan, which the quota tile does not show: it follows
// the pace of the consumption so far, others included).
export function planHint(plan: Plan | undefined, feasible: boolean, now = Date.now()): string {
  if (feasible) return "every critical job on schedule";
  const scope = plan?.scopes.find((s) => !s.feasible);
  if (!scope) return "a quota would run out before its reset";
  const name = scope.limit.replace(/^[^/]*\//, "");
  const when = scope.exhaustion_at ? ` ${since(scope.exhaustion_at, now)}` : "";
  return `the ${name} quota${scope.scope ? ` of ${scope.scope.replace(/=/g, " ").replace(/,/g, " and ")}` : ""} would run out${when} with the planned intervals`;
}

function Kpi({ icon, label, value, hint, ok, href: to }: { icon: "graph" | "jobs" | "budgets" | "plan"; label: string; value: string; hint: string; ok: boolean; href: string }) {
  return (
    <a className={`kpi-tile ${ok ? "ok" : "bad"}`} href={to}>
      <span className="ic"><Icon name={icon} /></span>
      <span className="label">{label}</span>
      <span className="value">{value}</span>
      <span className="hint">{hint}</span>
    </a>
  );
}

function ProviderCard({ provider: p, accounts, jobs, quotas }: { provider: Provider; accounts: Account[]; jobs: Job[]; quotas: Budget[] }) {
  const open = accounts.flatMap((a) => (a.targets.length ? a.targets.map((t) => t.circuit) : [a.circuit ?? "closed"])).filter((c) => c !== "closed").length;
  return (
    <div className="card provider-card">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <a className="row provider-name" href={href(["graph"])}>
          <span className="ic"><Icon name="braces" /></span>
          <span><strong>{p.name}</strong><span className="small muted"> {plural(accounts.length, "account")}</span></span>
        </a>
        <span className="row" style={{ gap: 8 }}>
          {open > 0 && <Badge state="degraded">{plural(open, "circuit")} open</Badge>}
          <Badge state={p.health} />
        </span>
      </div>
      <div>
        <div className="label">Jobs, by their last run</div>
        <Squares jobs={jobs} />
        <div className="small muted">{jobCounts(jobs)}</div>
      </div>
      {quotas.length > 0 && (
        <div className="provider-body">
          {quotas.map((q) => (
            <a key={q.limit + q.scope} className="quota" href={href(["budgets"], { limit: `${q.provider}/${q.limit}`, scope: q.scope })}>
              <div className="row" style={{ justifyContent: "space-between" }}>
                <span className="label">{q.limit} quota of {scopeAccounts(q)}</span>
                {q.exhaustion_eta_seconds !== undefined && <Badge state="failing">runs out in {duration(q.exhaustion_eta_seconds)}</Badge>}
              </div>
              <Meter total={q.max ?? 0} label={`${leftOf(q)} left of ${q.max}`} parts={[
                { value: q.used ?? 0, color: "var(--cyan-2)", label: `Tapyr ${count(q.used)}` },
                { value: q.external_usage ?? 0, color: "var(--sun)", label: `others ${count(q.external_usage)}` },
              ]} />
              <div className="small muted">{count(leftOf(q))} left of {count(q.max)}, resets <Since at={q.window_end} /></div>
            </a>
          ))}
        </div>
      )}
      {quotas.length === 0 && <div className="small muted">No quota: rate and concurrency limits only.</div>}
    </div>
  );
}

// squareWord says what a square stands for: the last run of the job, or
// what it waits for.
export function squareWord(j: Job, now = Date.now(), words?: (job: string) => string): string {
  const st = jobState(j);
  if (st === "ok") return "succeeded";
  if (st === "bad") return "failed";
  return j.waiting ? waitingText(j.waiting, now, words).short : "no run yet";
}

// Squares are the jobs as small squares colored by their last run; a
// failed one also carries a cross and a waiting one a dashed border, so
// that the color is not the only sign. The legend names the states shown.
function Squares({ jobs }: { jobs: Job[] }) {
  const label = jobLabels(jobs);
  const words = jobWords(jobs);
  const states = new Set(jobs.map(jobState));
  const legend = ([["ok", "succeeded"], ["bad", "failed"], ["idle", "waiting"]] as const).filter(([st]) => states.has(st));
  return (
    <>
      <div className="squares">
        {jobs.map((j) => {
          const word = `${label(j.name)}: ${squareWord(j, undefined, words)}`;
          return <a key={j.name} href={href(["jobs", j.name])} className={`sq ${jobState(j)}`} title={word} aria-label={word} />;
        })}
      </div>
      {legend.length > 1 && (
        <div className="sq-legend small muted" aria-hidden="true">
          {legend.map(([st, word]) => <span key={st}><span className={`sq ${st}`} /> {word}</span>)}
        </div>
      )}
    </>
  );
}

interface Activity {
  key: string;
  at: string;
  ok: boolean;
  title: string;
  detail: string;
}

// useActivity is the live feed: the last runs of the jobs at first, then
// every event of the stream.
function useActivity(jobs: Job[] | undefined, max = 14): Activity[] {
  const { subscribe } = useLive();
  const [live, setLive] = useState<Activity[]>([]);
  useEffect(
    () =>
      subscribe((e: LiveEvent) => {
        const d = e.data as Record<string, string | number | boolean>;
        const at = String(d.time ?? new Date().toISOString());
        let a: Activity;
        switch (e.type) {
          case "run_finished":
            a = { key: `${d.instance}-${d.run_id}`, at, ok: d.result === "success", title: String(d.job), detail: d.result === "success" ? `${d.samples} samples in ${d.duration_ms} ms` : d.problem ? String(d.problem) : `failed: ${d.result}` };
            break;
          case "config_reloaded":
            a = { key: `reload-${at}`, at, ok: d.result === "success", title: d.result === "success" ? "Configuration reloaded" : "Reload rejected", detail: d.result === "success" ? "success" : `${d.error ?? d.result}. The previous configuration keeps running.` };
            break;
          case "plan_changed":
            a = { key: `plan-${at}`, at, ok: d.feasible !== false, title: "Plan changed", detail: d.feasible === false ? "does not fit the quotas" : "fits the quotas" };
            break;
          default:
            a = { key: `circuit-${at}`, at, ok: d.state === "closed", title: `Circuit ${d.state}`, detail: `${d.account}${d.target ? "/" + d.target : ""}` };
        }
        setLive((l) => [a, ...l.filter((x) => x.key !== a.key)].slice(0, max));
      }),
    [subscribe, max],
  );
  const initial: Activity[] = (jobs ?? [])
    .filter((j) => j.last_run)
    .map((j) => ({ key: `${j.last_run!.instance}-${j.last_run!.run_id}`, at: j.last_run!.ended_at, ok: j.last_run!.result === "success", title: j.name, detail: j.last_run!.result === "success" ? "succeeded" : (j.last_run!.problem?.title ?? `failed: ${j.last_run!.error ?? j.last_run!.result}`) }))
    .sort((a, b) => b.at.localeCompare(a.at));
  const seen = new Set(live.map((a) => a.key));
  return [...live, ...initial.filter((a) => !seen.has(a.key))].slice(0, max);
}
