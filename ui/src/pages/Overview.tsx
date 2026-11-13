import { useEffect, useState } from "react";
import { Badge, Failure, Loading, Meter, PageHead } from "../components/ui";
import { Icon } from "../components/Icon";
import { count, duration, outputHealth, percent, plural, since } from "../lib/format";
import { useJSON, useList, useLive, type LiveEvent } from "../lib/live";
import { href } from "../lib/router";
import type { Account, Budget, Job, Output, Provider, Status } from "../lib/types";

type JobState = "ok" | "bad" | "idle";

const jobState = (j: Job): JobState => (!j.last_run ? "idle" : j.last_run.result === "success" ? "ok" : "bad");
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
  const activity = useActivity(jobs.data);
  if (status.error && !status.data) return <Failure error={status.error} />;
  if (!status.data || !providers.data || !accounts.data || !budgets.data || !jobs.data || !outputs.data) return <Loading />;
  const s = status.data;
  const quotas = budgets.data.filter((b) => b.kind === "quota");
  const tightest = [...quotas].sort((a, b) => leftOf(a) / (a.max || 1) - leftOf(b) / (b.max || 1))[0];
  const okProviders = providers.data.filter((p) => p.health === "ok").length;
  const okJobs = jobs.data.filter((j) => jobState(j) === "ok").length;
  const failing = jobs.data.filter((j) => jobState(j) === "bad");
  const reauth = accounts.data.filter((a) => a.auth.reauth_required);
  const unhealthy = unhealthyAccounts(accounts.data);
  return (
    <>
      <PageHead title="Overview" sub={`Tapyr ${s.version}, up ${duration(s.uptime_seconds)}, configuration ${s.config_hash.slice(0, 8)} loaded ${since(s.last_reload.at)}.`} />
      <div className="kpis">
        <Kpi icon="graph" label="Providers healthy" value={`${okProviders}/${providers.data.length}`} ok={okProviders === providers.data.length}
          hint={plural(accounts.data.length, "account")} href={href(["graph"])} />
        <Kpi icon="jobs" label="Jobs succeeding" value={`${okJobs}/${jobs.data.length}`} ok={failing.length === 0}
          hint={plural(s.instances, "instance")} href={href(["jobs"])} />
        <Kpi icon="budgets" label="Tightest quota" value={tightest ? percent(leftOf(tightest), tightest.max ?? 0) + " left" : "No quota"}
          ok={!tightest || tightest.exhaustion_eta_seconds === undefined}
          hint={tightest ? quotaHint(tightest) : "rate and concurrency limits only"} href={href(["budgets"])} />
        <Kpi icon="plan" label="Plan" value={s.plan_feasible ? "Fits" : "Does not fit"} ok={s.plan_feasible}
          hint={s.plan_feasible ? "every critical job on schedule" : "a quota would run out before its reset"} href={href(["plan"])} />
      </div>

      {(!s.ready || !s.last_reload.success || reauth.length > 0 || unhealthy.length > 0) && (
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
              <span className="hint">{since(s.last_reload.at)}: {s.last_reload.error}. The previous configuration keeps running.</span>
            </div>
          )}
          {reauth.map((a) => (
            <div className="cell bad" key={a.name}>
              <span className="label">{a.provider}</span>
              <span className="value"><Icon name="alert" />Re-bootstrap required</span>
              <span className="hint">Account {a.name}: its credentials can no longer be renewed. Generate new ones and reload.</span>
            </div>
          ))}
          {unhealthy.map((a) => (
            <div className={`cell ${a.health === "failing" ? "bad" : "warn"}`} key={`health-${a.name}`}>
              <span className="label">{a.provider} / {a.name}</span>
              <span className="value"><Icon name="alert" />Account {a.health}</span>
              <span className="hint mono">{a.reasons.join("; ")}</span>
            </div>
          ))}
        </div>
      )}

      <div className="cockpit">
        <div className="stack">
          <h2>Providers</h2>
          {providers.data.map((p) => (
            <ProviderCard key={p.name} provider={p} accounts={accounts.data!.filter((a) => a.provider === p.name)}
              jobs={jobs.data!.filter((j) => j.provider === p.name)} quotas={quotas.filter((q) => q.provider === p.name)} />
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
                    <strong>{a.title}</strong>
                    <span className="muted small"> {since(a.at)}</span>
                    <div className="small muted mono">{a.ok ? a.detail : `failed: ${a.detail}`}</div>
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
                    <a href={href(["jobs", j.name])}><strong>{j.name}</strong></a>
                    <span className="muted small"> {since(j.last_run?.ended_at)}</span>
                    {j.last_run?.error && <div className="mono small muted">{j.last_run.error}</div>}
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
            return (
              <a key={o.name} className={`chip ${bad ? "bad" : "ok"}`} href={href(["outputs"])}>
                <Icon name="outputs" /> {o.name}
                <span className="muted">{bad ? "failing" : o.type === "prometheus" ? "/metrics" : `${count(o.sent)} sent`}</span>
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
export function unhealthyAccounts(accounts: Account[]): { name: string; provider: string; health: Account["health"]; reasons: string[] }[] {
  const rank = (h: string) => (h === "failing" ? 0 : 1);
  return accounts
    .filter((a) => a.health === "failing" || a.health === "degraded")
    .map((a) => ({
      name: a.name,
      provider: a.provider,
      health: a.health,
      reasons: a.auth.reauth_required ? (a.health_reasons ?? []).filter((r) => !r.includes("re-bootstrap")) : a.health_reasons ?? [],
    }))
    .filter((a) => a.reasons.length > 0)
    .sort((a, b) => rank(a.health) - rank(b.health) || a.name.localeCompare(b.name));
}

// quotaHint names a quota and when it resets, and, when it would run out
// before then at the current pace, when it runs out: the red state of the
// indicator then carries words.
export function quotaHint(b: Budget, now = Date.now()): string {
  const name = `${b.provider} / ${b.limit}`;
  if (b.exhaustion_eta_seconds !== undefined) return `${name}, runs out in ${duration(b.exhaustion_eta_seconds)}, resets ${since(b.window_end, now)}`;
  return `${name}, resets ${since(b.window_end, now)}`;
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
  const ok = jobs.filter((j) => jobState(j) === "ok").length;
  const bad = jobs.filter((j) => jobState(j) === "bad").length;
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
        <div className="small muted">{ok} of {plural(jobs.length, "job")} succeeded{bad ? `, ${bad} failing` : ""}</div>
      </div>
      {quotas.length > 0 && (
        <div className="provider-body">
          {quotas.map((q) => (
            <a key={q.limit + q.scope} className="quota" href={href(["budgets"], { limit: `${q.provider}/${q.limit}`, scope: q.scope })}>
              <div className="row" style={{ justifyContent: "space-between" }}>
                <span className="label">{q.limit} quota, {count(leftOf(q))} left of {count(q.max)}</span>
                {q.exhaustion_eta_seconds !== undefined && <Badge state="failing">runs out in {duration(q.exhaustion_eta_seconds)}</Badge>}
              </div>
              <Meter total={q.max ?? 0} label={`${leftOf(q)} left of ${q.max}`} parts={[
                { value: q.used ?? 0, color: "var(--cyan-2)", label: `Tapyr ${count(q.used)}` },
                { value: q.external_usage ?? 0, color: "var(--sun)", label: `others ${count(q.external_usage)}` },
              ]} />
              <div className="small muted">resets {since(q.window_end)}</div>
            </a>
          ))}
        </div>
      )}
      {quotas.length === 0 && <div className="small muted">No quota: rate and concurrency limits only.</div>}
    </div>
  );
}

// Squares are the jobs as small squares colored by their last run; a
// failed one also carries a cross, so that the color is not the only sign.
function Squares({ jobs }: { jobs: Job[] }) {
  const words = { ok: "succeeded", bad: "failed", idle: "not run yet" };
  return (
    <div className="squares">
      {jobs.map((j) => {
        const st = jobState(j);
        return <a key={j.name} href={href(["jobs", j.name])} className={`sq ${st}`} title={`${j.name}: ${words[st]}`} aria-label={`${j.name}: ${words[st]}`} />;
      })}
    </div>
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
            a = { key: `${d.instance}-${d.run_id}`, at, ok: d.result === "success", title: String(d.job), detail: d.result === "success" ? `${d.samples} samples in ${d.duration_ms} ms` : String(d.result) };
            break;
          case "config_reloaded":
            a = { key: `reload-${at}`, at, ok: d.result === "success", title: "Configuration reloaded", detail: String(d.error ?? d.result) };
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
    .map((j) => ({ key: `${j.last_run!.instance}-${j.last_run!.run_id}`, at: j.last_run!.ended_at, ok: j.last_run!.result === "success", title: j.name, detail: j.last_run!.result === "success" ? "succeeded" : (j.last_run!.error ?? j.last_run!.result) }))
    .sort((a, b) => b.at.localeCompare(a.at));
  const seen = new Set(live.map((a) => a.key));
  return [...live, ...initial.filter((a) => !seen.has(a.key))].slice(0, max);
}
