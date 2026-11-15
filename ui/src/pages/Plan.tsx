import { Badge, Failure, Loading, PageHead, Table } from "../components/ui";
import { ProblemNote } from "../components/Problem";
import { absolute, count, duration, percent, scopeWords, since, waitingText } from "../lib/format";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href } from "../lib/router";
import type { Job, Plan as PlanT, PlanJob, PlanScope } from "../lib/types";

// unplanned are the collected jobs the plan does not hold yet: fan-outs
// without instances, waiting for their parent or whose parent listed no
// item. The planner counts instances, so they join the plan once their
// parent has listed its items.
// blocking returns the critical jobs of the quotas the plan does not fit:
// they are never slowed down, so they are what to change.
export function blocking(plan: PlanT): Set<string> {
  const critical = new Set(plan.jobs.filter((j) => j.priority === "critical").map((j) => j.name));
  return new Set(plan.scopes.filter((s) => !s.feasible).flatMap((s) => s.jobs.filter((j) => critical.has(j))));
}

export function unplanned(plan: PlanT, jobs: Job[]): { name: string; why: string }[] {
  const planned = new Set(plan.jobs.map((j) => j.name.replace(/\[.*$/, "")));
  return jobs
    .filter((j) => j.collected && !planned.has(j.name))
    .map((j) => ({
      name: j.name,
      why: j.waiting ? waitingText(j.waiting).long : j.parent && j.instances === 0 ? `${j.parent} listed no item in its last run.` : "Not planned yet.",
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

export function Plan() {
  const plan = useJSON<PlanT>("/plan");
  const jobs = useList<Job>("/jobs");
  if (plan.error instanceof HttpError && plan.error.status === 404) {
    return (
      <>
        <PageHead title="Plan" />
        <div className="muted">No plan applied yet: the first one comes with the first runs.</div>
      </>
    );
  }
  if (plan.error && !plan.data) return <Failure error={plan.error} />;
  if (!plan.data) return <Loading />;
  const p = plan.data;
  const unfit = p.scopes.filter((sc) => !sc.feasible && sc.problem);
  const culprits = blocking(p);
  const missing = jobs.data ? unplanned(p, jobs.data) : [];
  return (
    <>
      <PageHead title="Plan" sub={<span title={absolute(p.at)}>Applied {since(p.at)}: what each quota will spend until its reset, and the jobs slowed down to fit. <span className="mono">tapyr plan</span> projects a full window instead.</span>}>
        <Badge state={p.feasible ? "ok" : "failing"}>{p.feasible ? "fits" : "does not fit"}</Badge>
      </PageHead>
      {unfit.length > 0 && (
        <div className="card why">
          <h2 style={{ marginTop: 0 }}>Why it does not fit</h2>
          {unfit.map((sc) => (
            <div key={`${sc.limit}|${sc.scope}`} className="why-scope">
              <ProblemNote problem={sc.problem} />
            </div>
          ))}
          <p className="small muted" style={{ marginBottom: 0 }}>
            Critical jobs are never slowed down: they are marked below. The other jobs are slowed down at most as <span className="mono">settings.budget.maxStretch</span> allows.
          </p>
        </div>
      )}
      <section className="section">
        <h2>Quota scopes</h2>
        <Table<PlanScope>
          rows={p.scopes}
          rowKey={(s) => `${s.limit}|${s.scope}`}
          initialSort={{ key: "limit" }}
          highlight={(s) => !s.feasible}
          empty="No quota limit applies to the configured jobs."
          columns={[
            { key: "limit", label: "Quota", sort: (s) => s.limit, render: (s) => <strong>{s.limit}</strong> },
            { key: "scope", label: "Of", render: (s) => <span className="small" title={s.scope}>{scopeWords(s.scope)}</span> },
            { key: "period", label: "Until reset", num: true, sort: (s) => s.period_seconds, render: (s) => duration(s.period_seconds) },
            { key: "budget", label: "Available", num: true, sort: (s) => s.budget, render: (s) => <span title="What the quota leaves to Tapyr until its reset, the calls kept for retries left out">{count(s.budget)}</span> },
            { key: "projected", label: "Projected", num: true, sort: (s) => s.projected, render: (s) => <span title="What the jobs will spend until the reset with the planned intervals">{count(s.projected)}</span> },
            { key: "reserve", label: "For retries", num: true, render: (s) => <span title="Calls kept for retries and critical jobs">{count(s.reserve)}</span> },
            { key: "use", label: "Use", num: true, sort: (s) => s.projected / (s.budget || 1), render: (s) => percent(s.projected, s.budget) },
            {
              key: "status", label: "Status", sort: (s) => (s.feasible ? 1 : 0),
              render: (s) => (s.feasible ? <Badge state="ok">fits</Badge> : (
                <span title="with the planned intervals"><Badge state="failing">does not fit</Badge>{s.exhaustion_at && <span className="small muted"> runs out {since(s.exhaustion_at)}</span>}</span>
              )),
            },
          ]}
        />
      </section>
      <section className="section">
        <h2>Jobs</h2>
        <Table<PlanJob>
          rows={p.jobs}
          rowKey={(j) => j.name}
          initialSort={{ key: "stretch", desc: true }}
          highlight={(j) => j.stretch > 1 || culprits.has(j.name)}
          columns={[
            { key: "name", label: "Job", sort: (j) => j.name, render: (j) => <a href={href(["jobs", j.name.replace(/\[.*$/, "")])}>{j.name}</a> },
            { key: "account", label: "Account", sort: (j) => j.account, render: (j) => j.account },
            {
              key: "priority", label: "Priority", sort: (j) => ["critical", "high", "normal", "low"].indexOf(j.priority),
              render: (j) => (culprits.has(j.name) ? <span title="Critical jobs are never slowed down: this one does not fit its quota."><Badge state="failing">critical</Badge></span> : j.priority),
            },
            { key: "desired", label: "Desired", num: true, sort: (j) => j.desired_interval_seconds, render: (j) => duration(j.desired_interval_seconds) },
            { key: "effective", label: "Effective", num: true, sort: (j) => j.effective_interval_seconds, render: (j) => duration(j.effective_interval_seconds) },
            { key: "stretch", label: "Slowed down", num: true, sort: (j) => (culprits.has(j.name) ? Infinity : j.stretch), render: (j) => (j.stretch > 1 ? <strong>x{+j.stretch.toFixed(2)}</strong> : "no") },
            { key: "instances", label: "Instances", num: true, sort: (j) => j.instances, render: (j) => count(j.instances) },
            { key: "pages", label: "Pages per run", num: true, sort: (j) => j.pages_per_run, render: (j) => +j.pages_per_run.toFixed(2) },
            { key: "reason", label: "Reason", render: (j) => <span className="small muted">{culprits.has(j.name) ? "critical, never slowed down: does not fit its quota" : j.reason ?? ""}</span> },
          ]}
        />
      </section>
      {missing.length > 0 && (
        <section className="section">
          <h2>Not planned yet</h2>
          <p className="small muted">Fan-outs without instances: the plan counts them once their parent has listed its items.</p>
          <ul className="small">
            {missing.map((m) => (
              <li key={m.name}><a href={href(["jobs", m.name])}>{m.name}</a>: {m.why}</li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}
