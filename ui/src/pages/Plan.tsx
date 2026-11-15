import { Badge, Failure, Loading, PageHead, Table } from "../components/ui";
import { absolute, count, duration, percent, since, waitingText } from "../lib/format";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href } from "../lib/router";
import type { Job, Plan as PlanT, PlanJob, PlanScope } from "../lib/types";

// unplanned are the collected jobs the plan does not hold yet: fan-outs
// without instances, waiting for their parent or whose parent listed no
// item. The planner counts instances, so they join the plan once their
// parent has listed its items.
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
  const missing = jobs.data ? unplanned(p, jobs.data) : [];
  return (
    <>
      <PageHead title="Plan" sub={<span title={absolute(p.at)}>Applied {since(p.at)}: what each quota scope will spend over a full window, and the jobs stretched to fit.</span>}>
        <Badge state={p.feasible ? "ok" : "failing"}>{p.feasible ? "feasible" : "infeasible"}</Badge>
      </PageHead>
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
            { key: "scope", label: "Scope", render: (s) => <span className="mono small">{s.scope}</span> },
            { key: "period", label: "Window left", num: true, sort: (s) => s.period_seconds, render: (s) => duration(s.period_seconds) },
            { key: "budget", label: "Budget", num: true, sort: (s) => s.budget, render: (s) => count(s.budget) },
            { key: "projected", label: "Projected", num: true, sort: (s) => s.projected, render: (s) => count(s.projected) },
            { key: "reserve", label: "Reserve", num: true, render: (s) => count(s.reserve) },
            { key: "use", label: "Use", num: true, sort: (s) => s.projected / (s.budget || 1), render: (s) => percent(s.projected, s.budget) },
            {
              key: "status", label: "Status", sort: (s) => (s.feasible ? 1 : 0),
              render: (s) => (s.feasible ? <Badge state="ok">ok</Badge> : <Badge state="failing">exhausted {s.exhaustion_at ? since(s.exhaustion_at) : ""}</Badge>),
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
          highlight={(j) => j.stretch > 1}
          columns={[
            { key: "name", label: "Job", sort: (j) => j.name, render: (j) => <a href={href(["jobs", j.name.replace(/\[.*$/, "")])}>{j.name}</a> },
            { key: "account", label: "Account", sort: (j) => j.account, render: (j) => j.account },
            { key: "priority", label: "Priority", sort: (j) => ["critical", "high", "normal", "low"].indexOf(j.priority), render: (j) => j.priority },
            { key: "desired", label: "Desired", num: true, sort: (j) => j.desired_interval_seconds, render: (j) => duration(j.desired_interval_seconds) },
            { key: "effective", label: "Effective", num: true, sort: (j) => j.effective_interval_seconds, render: (j) => duration(j.effective_interval_seconds) },
            { key: "stretch", label: "Stretch", num: true, sort: (j) => j.stretch, render: (j) => (j.stretch > 1 ? <strong>x{j.stretch.toFixed(2)}</strong> : "x1") },
            { key: "instances", label: "Instances", num: true, sort: (j) => j.instances, render: (j) => count(j.instances) },
            { key: "pages", label: "Pages per run", num: true, sort: (j) => j.pages_per_run, render: (j) => +j.pages_per_run.toFixed(2) },
            { key: "reason", label: "Reason", render: (j) => <span className="small muted">{j.reason ?? ""}</span> },
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
