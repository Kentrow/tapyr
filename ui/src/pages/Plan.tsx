import { Badge, Failure, Loading, PageHead, Table } from "../components/ui";
import { ProblemNote } from "../components/Problem";
import { JobName } from "../components/JobName";
import { absolute, count, duration, jobWords, percent, scopeWords, since, waitingText } from "../lib/format";
import { terms } from "../lib/terms";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href } from "../lib/router";
import type { Job, Plan as PlanT, PlanJob, PlanScope } from "../lib/types";

// blocking returns the critical jobs of the quotas the plan does not fit:
// they are never slowed down, so they are what to change.
export function blocking(plan: PlanT): Set<string> {
  const critical = new Set(plan.jobs.filter((j) => j.priority === "critical").map((j) => j.name));
  return new Set(plan.scopes.filter((s) => !s.feasible).flatMap((s) => s.jobs.filter((j) => critical.has(j))));
}

// unplanned are the collected jobs the plan does not hold yet: fan-outs
// without instances, waiting for their parent or whose parent listed no
// item. The planner counts instances, so they join the plan once their
// parent has listed its items.
export function unplanned(plan: PlanT, jobs: Job[], now = Date.now()): { name: string; why: string }[] {
  const planned = new Set(plan.jobs.map((j) => j.name.replace(/\[.*$/, "")));
  const words = jobWords(jobs);
  return jobs
    .filter((j) => j.collected && !planned.has(j.name))
    .map((j) => ({
      name: j.name,
      why: j.waiting ? waitingText(j.waiting, now, words).long : j.parent && j.instances === 0 ? `${subject(words, j.parent)} listed no item in its last run.` : "Not planned yet.",
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

// subject starts a sentence with a job: "The job listing the sites" for an
// implicit one, the name as it is of the others.
function subject(words: (job: string) => string, name: string): string {
  const w = words(name);
  return w === name ? w : w.charAt(0).toUpperCase() + w.slice(1);
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
            { key: "limit", label: "Quota", help: terms.quota, sort: (s) => s.limit, render: (s) => <strong>{s.limit}</strong> },
            { key: "scope", label: "Of", help: terms.scope, render: (s) => <span className="small" title={s.scope}>{scopeWords(s.scope)}</span> },
            { key: "period", label: "Until reset", help: terms.untilReset, num: true, sort: (s) => s.period_seconds, render: (s) => duration(s.period_seconds) },
            { key: "budget", label: "Available", help: terms.available, num: true, sort: (s) => s.budget, render: (s) => count(s.budget) },
            { key: "projected", label: "Projected", help: terms.projected, num: true, sort: (s) => s.projected, render: (s) => count(s.projected) },
            { key: "reserve", label: "For retries", help: terms.reserve, num: true, render: (s) => count(s.reserve) },
            { key: "use", label: "Use", help: terms.use, num: true, sort: (s) => s.projected / (s.budget || 1), render: (s) => percent(s.projected, s.budget) },
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
            { key: "name", label: "Job", sort: (j) => j.name, render: (j) => <PlanJobName name={j.name} jobs={jobs.data} /> },
            { key: "account", label: "Account", sort: (j) => j.account, render: (j) => j.account },
            {
              key: "priority", label: "Priority", help: terms.priority, sort: (j) => ["critical", "high", "normal", "low"].indexOf(j.priority),
              render: (j) => (culprits.has(j.name) ? <span title="Critical jobs are never slowed down: this one does not fit its quota."><Badge state="failing">critical</Badge></span> : j.priority),
            },
            { key: "desired", label: "Desired", help: terms.desired, num: true, sort: (j) => j.desired_interval_seconds, render: (j) => duration(j.desired_interval_seconds) },
            { key: "effective", label: "Effective", help: terms.effective, num: true, sort: (j) => j.effective_interval_seconds, render: (j) => duration(j.effective_interval_seconds) },
            { key: "stretch", label: "Slowed down", help: terms.stretch, num: true, sort: (j) => (culprits.has(j.name) ? Infinity : j.stretch), render: (j) => (j.stretch > 1 ? <strong>x{+j.stretch.toFixed(2)}</strong> : "no") },
            { key: "instances", label: "Instances", help: terms.instances, num: true, sort: (j) => j.instances, render: (j) => count(j.instances) },
            { key: "pages", label: "Pages per run", help: terms.pages, num: true, sort: (j) => j.pages_per_run, render: (j) => +j.pages_per_run.toFixed(2) },
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
              <li key={m.name}><JobName compact name={m.name} job={jobs.data?.find((j) => j.name === m.name)} />: {m.why}</li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}

// PlanJobName names a job of the plan, whose name can carry the target
// of its group (devices[target=north]).
function PlanJobName({ name, jobs }: { name: string; jobs?: Job[] }) {
  const base = name.replace(/\[.*$/, "");
  const job = jobs?.find((j) => j.name === base);
  if (base !== name || !job) return <a href={href(["jobs", base])}>{name}</a>;
  return <JobName name={name} job={job} />;
}
