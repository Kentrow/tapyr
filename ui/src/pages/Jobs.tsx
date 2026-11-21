import { Badge, Failure, Help, Loading, NextRun, PageHead, Since, Table } from "../components/ui";
import { CodeBlock } from "../components/Code";
import { ProblemNote } from "../components/Problem";
import { NotFound } from "./NotFound";
import { JobName } from "../components/JobName";
import { absolute, count, duration, implicitHelp, jobTitle, jobWords, toYAML, waitingText } from "../lib/format";
import { terms } from "../lib/terms";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href } from "../lib/router";
import type { Instance, Job, List, Run } from "../lib/types";

export function Jobs() {
  const jobs = useList<Job>("/jobs");
  if (jobs.error && !jobs.data) return <Failure error={jobs.error} />;
  if (!jobs.data) return <Loading />;
  const words = jobWords(jobs.data);
  return (
    <>
      <PageHead title="Jobs" sub="Every job with its desired and effective interval. Tapyr adds the jobs that list the items of a fan-out when no job of the configuration lists them; jobs not collected make no API call (no output keeps their metrics)." />
      <Table<Job>
        rows={jobs.data}
        rowKey={(j) => j.name}
        initialSort={{ key: "name" }}
        highlight={(j) => j.effective_interval_seconds > j.desired_interval_seconds}
        columns={[
          {
            key: "name", label: "Job", sort: (j) => j.name,
            render: (j) => (
              <span>
                <JobName name={j.name} job={j} />
                {!j.collected && <span title={j.not_collected_reason}> <Badge state="unknown">not collected</Badge></span>}
              </span>
            ),
          },
          { key: "account", label: "Account", sort: (j) => j.account, render: (j) => j.account },
          { key: "role", label: "Role", help: terms.role, sort: (j) => j.role, render: (j) => j.role },
          { key: "instances", label: "Instances", help: terms.instances, num: true, sort: (j) => j.instances, render: (j) => count(j.instances) },
          {
            key: "interval", label: "Interval", help: terms.effective, num: true, sort: (j) => j.effective_interval_seconds,
            render: (j) =>
              !j.collected ? (
                <span className="muted" title={j.not_collected_reason}>-</span>
              ) : j.effective_interval_seconds > j.desired_interval_seconds ? (
                <span title={j.stretch_reason}>
                  {duration(j.effective_interval_seconds)} <span className="muted small">(desired {duration(j.desired_interval_seconds)})</span>
                </span>
              ) : (
                duration(j.desired_interval_seconds)
              ),
          },
          {
            key: "last", label: "Last run", sort: (j) => j.last_run?.ended_at ?? "",
            render: (j) =>
              j.last_run ? (
                <span><Badge state={j.last_run.result} /> <Since at={j.last_run.ended_at} /></span>
              ) : j.waiting ? (
                <span className="muted" title={waitingText(j.waiting, undefined, words).long}>{waitingText(j.waiting, undefined, words).short}</span>
              ) : (
                <span className="muted">{j.collected ? "no run yet" : "-"}</span>
              ),
          },
          {
            key: "next", label: "Next run", sort: (j) => j.next_run ?? j.waiting?.until ?? "",
            render: (j) =>
              j.next_run ? (
                <NextRun at={j.next_run} />
              ) : j.waiting?.until ? (
                <span className="muted" title={`${absolute(j.waiting.until)}: the next step of its discovery; the job runs after it`}>discovery <NextRun at={j.waiting.until} /></span>
              ) : (
                "-"
              ),
          },
        ]}
      />
    </>
  );
}

// instancesEmpty says why a job has no instance: not collected, or a
// fan-out whose parent has not listed its items yet, fails, or listed
// none.
export function instancesEmpty(j: Job, words: (job: string) => string = (job) => job): string {
  if (!j.collected) return "No instance: the job is not collected.";
  const w = j.waiting;
  if (w?.reason === "parent") return `No instance yet: ${words(w.parent ?? "")} has not listed its items.`;
  if (w?.reason === "parent_failing") {
    const parent = words(w.parent ?? "");
    return `No instance yet: ${w.failing_job === w.parent ? parent : `${parent} waits for ${words(w.failing_job ?? "")}, which`} fails.`;
  }
  if (j.parent) return `No instance: ${words(j.parent)} listed no item in its last run.`;
  return "No instance.";
}

export function JobDetail({ name }: { name: string }) {
  const job = useJSON<Job>(`/jobs/${encodeURIComponent(name)}`);
  const instances = useList<Instance>(`/jobs/${encodeURIComponent(name)}/instances`);
  const runs = useJSON<List<Run>>(`/jobs/${encodeURIComponent(name)}/runs?limit=25`);
  const jobs = useList<Job>("/jobs");
  if (job.error instanceof HttpError && job.error.status === 404) return <NotFound what={`No job ${name} in the running configuration.`} />;
  if (job.error && !job.data) return <Failure error={job.error} />;
  if (!job.data) return <Loading />;
  const j = job.data;
  const stretched = j.collected && j.effective_interval_seconds > j.desired_interval_seconds;
  const words = jobWords(jobs.data);
  const byName = (n: string) => jobs.data?.find((o) => o.name === n);
  const t = jobTitle(j);
  const where = `${j.provider} / ${j.account}${j.template ? `, template ${j.template}` : ""}`;
  return (
    <>
      <PageHead title={t.title} sub={t.detail ? <><abbr className="help" title={implicitHelp}>{t.detail}</abbr>: job <span className="mono">{j.name}</span>, {where}</> : where}>
        <a className="btn" href={href(["debug"], { job: j.name })}>Live debug</a>
      </PageHead>
      <div className="grid">
        <div className="card">
          <h3>Interval</h3>
          {!j.collected ? (
            <>
              <span className="kpi"><Badge state="unknown">not collected</Badge></span>
              <p className="small" style={{ margin: "6px 0 0", overflowWrap: "anywhere" }}>
                No API call: {j.not_collected_reason}. <span className="muted">See the processing rules of the outputs, or <span className="mono">settings.collection.skipUnconsumed</span>.</span>
              </p>
            </>
          ) : (
            <span className="kpi">{duration(j.effective_interval_seconds)}</span>
          )}
          {!j.collected ? null : stretched ? (
            <p className="small" style={{ margin: "6px 0 0" }}>
              <Badge state="degraded">stretched</Badge> desired {duration(j.desired_interval_seconds)}: {j.stretch_reason}
            </p>
          ) : (
            <p className="small muted" style={{ margin: "6px 0 0" }}>as desired</p>
          )}
        </div>
        <div className="card">
          <h3>Last run</h3>
          {j.last_run ? (
            <>
              <div className="row"><Badge state={j.last_run.result} /> <Since at={j.last_run.ended_at} /></div>
              {(j.last_run.problem || j.last_run.error) && (
                <div className="small" style={{ marginTop: 8 }}><ProblemNote problem={j.last_run.problem} detail={j.last_run.error} /></div>
              )}
            </>
          ) : j.waiting ? (
            <>
              <div className="row"><Badge state={j.waiting.reason === "parent_failing" ? "degraded" : "unknown"}>waiting</Badge></div>
              <p className="small" style={{ overflowWrap: "anywhere", marginBottom: 0 }}>
                {waitingText(j.waiting, undefined, words).long}
                {j.waiting.failing_job && <> See <JobName compact name={j.waiting.failing_job} job={byName(j.waiting.failing_job)} />.</>}
              </p>
              {j.waiting.problem && <div className="small" style={{ marginTop: 8 }}><ProblemNote problem={j.waiting.problem} compact /></div>}
            </>
          ) : (
            <span className="muted">{j.collected ? "no run yet" : "none: not collected"}</span>
          )}
          {j.collected && j.next_run && <p className="small muted" style={{ marginBottom: 0 }}>next <NextRun at={j.next_run} /></p>}
        </div>
        <div className="card">
          <h3>Job</h3>
          <dl className="kv small">
            <dt><Help text={terms.role}>Role</Help></dt><dd>{j.role}</dd>
            <dt><Help text={terms.priority}>Priority</Help></dt><dd>{j.priority}</dd>
            {j.parent && (<><dt>Parent</dt><dd><JobName compact name={j.parent} job={byName(j.parent)} /></dd></>)}
            <dt><Help text={terms.instances}>Instances</Help></dt><dd>{count(j.instances)}</dd>
            <dt>Outputs</dt><dd>{j.outputs.length ? j.outputs.join(", ") : "all"}</dd>
          </dl>
        </div>
      </div>

      <section className="section">
        <h2>Recent runs</h2>
        <Table<Run>
          rows={runs.data?.items ?? []}
          rowKey={(r) => r.run_id + r.instance}
          empty="No run yet."
          columns={[
            { key: "started", label: "Started", render: (r) => <Since at={r.started_at} /> },
            { key: "instance", label: "Instance", render: (r) => <span className="mono small">{r.instance}</span> },
            { key: "result", label: "Result", render: (r) => <Badge state={r.result} /> },
            { key: "duration", label: "Duration", num: true, render: (r) => `${r.duration_seconds < 1 ? Math.round(r.duration_seconds * 1000) + "ms" : duration(r.duration_seconds)}` },
            { key: "requests", label: "Requests", num: true, render: (r) => r.requests },
            { key: "pages", label: "Pages", num: true, render: (r) => r.pages },
            { key: "retries", label: "Retries", num: true, render: (r) => r.retries },
            { key: "samples", label: "Samples", num: true, render: (r) => count(r.samples) },
            { key: "error", label: "Error", render: (r) => (r.problem ? <span className="small" title={r.error}>{r.problem.title}</span> : <span className="mono small" style={{ overflowWrap: "anywhere" }}>{r.error ?? ""}</span>) },
          ]}
        />
      </section>

      <section className="section">
        <h2>Instances</h2>
        <Table<Instance>
          rows={instances.data ?? []}
          rowKey={(i) => i.key}
          initialSort={{ key: "key" }}
          empty={instancesEmpty(j, words)}
          columns={[
            { key: "key", label: "Instance", sort: (i) => i.key, render: (i) => <span className="mono small">{i.key}</span> },
            { key: "vars", label: "Variables", render: (i) => <span className="mono small">{Object.entries(i.vars).map(([k, v]) => `${k}=${v}`).join(" ")}</span> },
            { key: "series", label: "Series", num: true, sort: (i) => i.series, render: (i) => count(i.series) },
            { key: "last", label: "Last run", sort: (i) => i.last_run?.ended_at ?? "", render: (i) => (i.last_run ? <><Badge state={i.last_run.result} /> <Since at={i.last_run.ended_at} /></> : <span className="muted">{i.next_run ? <>first run <NextRun at={i.next_run} /></> : "no run yet"}</span>) },
            { key: "success", label: "Last success", sort: (i) => i.last_success ?? "", render: (i) => <Since at={i.last_success} /> },
            { key: "next", label: "Next run", sort: (i) => i.next_run ?? "", render: (i) => <NextRun at={i.next_run} /> },
          ]}
        />
      </section>

      {j.definition !== undefined && (
        <section className="section">
          <h2>Effective definition</h2>
          <CodeBlock code={toYAML(j.definition).trimStart()} lang="yaml" />
        </section>
      )}
    </>
  );
}
