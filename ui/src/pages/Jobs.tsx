import { Badge, Failure, Loading, PageHead, Table } from "../components/ui";
import { CodeBlock } from "../components/Code";
import { NotFound } from "./NotFound";
import { absolute, count, duration, since, toYAML } from "../lib/format";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href } from "../lib/router";
import type { Instance, Job, List, Run } from "../lib/types";

export function Jobs() {
  const jobs = useList<Job>("/jobs");
  if (jobs.error && !jobs.data) return <Failure error={jobs.error} />;
  if (!jobs.data) return <Loading />;
  return (
    <>
      <PageHead title="Jobs" sub="Every job with its desired and effective interval; implicit jobs are the parents of fan-outs that no job configures, and jobs not collected make no API call (no output keeps their metrics)." />
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
                <a href={href(["jobs", j.name])}>{j.name}</a>
                {j.implicit && <span className="muted small"> (implicit)</span>}
                {!j.collected && <span title={j.not_collected_reason}> <Badge state="unknown">not collected</Badge></span>}
              </span>
            ),
          },
          { key: "account", label: "Account", sort: (j) => j.account, render: (j) => j.account },
          { key: "role", label: "Role", sort: (j) => j.role, render: (j) => j.role },
          { key: "instances", label: "Instances", num: true, sort: (j) => j.instances, render: (j) => count(j.instances) },
          {
            key: "interval", label: "Interval", num: true, sort: (j) => j.effective_interval_seconds,
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
          { key: "last", label: "Last run", sort: (j) => j.last_run?.ended_at ?? "", render: (j) => (j.last_run ? <span title={absolute(j.last_run.ended_at)}><Badge state={j.last_run.result} /> {since(j.last_run.ended_at)}</span> : <span className="muted">not yet</span>) },
          { key: "next", label: "Next run", sort: (j) => j.next_run ?? "", render: (j) => <span title={absolute(j.next_run)}>{j.next_run ? since(j.next_run) : "-"}</span> },
        ]}
      />
    </>
  );
}

export function JobDetail({ name }: { name: string }) {
  const job = useJSON<Job>(`/jobs/${encodeURIComponent(name)}`);
  const instances = useList<Instance>(`/jobs/${encodeURIComponent(name)}/instances`);
  const runs = useJSON<List<Run>>(`/jobs/${encodeURIComponent(name)}/runs?limit=25`);
  if (job.error instanceof HttpError && job.error.status === 404) return <NotFound what={`No job ${name} in the running configuration.`} />;
  if (job.error && !job.data) return <Failure error={job.error} />;
  if (!job.data) return <Loading />;
  const j = job.data;
  const stretched = j.collected && j.effective_interval_seconds > j.desired_interval_seconds;
  return (
    <>
      <PageHead title={j.name} sub={`${j.provider} / ${j.account}${j.template ? `, template ${j.template}` : ""}`}>
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
              <div className="row"><Badge state={j.last_run.result} /> <span title={absolute(j.last_run.ended_at)}>{since(j.last_run.ended_at)}</span></div>
              {j.last_run.error && <p className="mono small" style={{ overflowWrap: "anywhere" }}>{j.last_run.error}</p>}
            </>
          ) : (
            <span className="muted">not yet</span>
          )}
          {j.collected && <p className="small muted" style={{ marginBottom: 0 }}>next {since(j.next_run)}</p>}
        </div>
        <div className="card">
          <h3>Job</h3>
          <dl className="kv small">
            <dt>Role</dt><dd>{j.role}</dd>
            <dt>Priority</dt><dd>{j.priority}</dd>
            {j.parent && (<><dt>Parent</dt><dd><a href={href(["jobs", j.parent])}>{j.parent}</a></dd></>)}
            <dt>Instances</dt><dd>{count(j.instances)}</dd>
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
            { key: "started", label: "Started", render: (r) => <span title={absolute(r.started_at)}>{since(r.started_at)}</span> },
            { key: "instance", label: "Instance", render: (r) => <span className="mono small">{r.instance}</span> },
            { key: "result", label: "Result", render: (r) => <Badge state={r.result} /> },
            { key: "duration", label: "Duration", num: true, render: (r) => `${r.duration_seconds < 1 ? Math.round(r.duration_seconds * 1000) + "ms" : duration(r.duration_seconds)}` },
            { key: "requests", label: "Requests", num: true, render: (r) => r.requests },
            { key: "pages", label: "Pages", num: true, render: (r) => r.pages },
            { key: "retries", label: "Retries", num: true, render: (r) => r.retries },
            { key: "samples", label: "Samples", num: true, render: (r) => count(r.samples) },
            { key: "error", label: "Error", render: (r) => <span className="mono small" style={{ overflowWrap: "anywhere" }}>{r.error ?? ""}</span> },
          ]}
        />
      </section>

      <section className="section">
        <h2>Instances</h2>
        <Table<Instance>
          rows={instances.data ?? []}
          rowKey={(i) => i.key}
          initialSort={{ key: "key" }}
          empty={j.collected ? "No instance: the parent inventory has not run yet." : "No instance: the job is not collected."}
          columns={[
            { key: "key", label: "Instance", sort: (i) => i.key, render: (i) => <span className="mono small">{i.key}</span> },
            { key: "vars", label: "Variables", render: (i) => <span className="mono small">{Object.entries(i.vars).map(([k, v]) => `${k}=${v}`).join(" ")}</span> },
            { key: "series", label: "Series", num: true, sort: (i) => i.series, render: (i) => count(i.series) },
            { key: "last", label: "Last run", sort: (i) => i.last_run?.ended_at ?? "", render: (i) => (i.last_run ? <><Badge state={i.last_run.result} /> {since(i.last_run.ended_at)}</> : <span className="muted">not yet</span>) },
            { key: "success", label: "Last success", sort: (i) => i.last_success ?? "", render: (i) => <span title={absolute(i.last_success)}>{since(i.last_success)}</span> },
            { key: "next", label: "Next run", sort: (i) => i.next_run ?? "", render: (i) => since(i.next_run) },
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
