import { Fragment, useMemo } from "react";
import { Badge, Failure, Loading, PageHead, Select, Table } from "../components/ui";
import { JsonTree } from "../components/JsonTree";
import { CodeBlock, CopyButton } from "../components/Code";
import { ProblemNote } from "../components/Problem";
import { absolute, since } from "../lib/format";
import { useJSON, useList } from "../lib/live";
import { HttpError } from "../lib/api";
import { href, useRoute } from "../lib/router";
import type { Debug as DebugT, DebugSample, Job } from "../lib/types";

export function Debug() {
  const route = useRoute();
  const jobs = useList<Job>("/jobs");
  const job = route.query.get("job") ?? "";
  const instance = route.query.get("instance") ?? "";
  const capture = useJSON<DebugT>(job ? `/debug/jobs/${encodeURIComponent(job)}/last${instance ? `?instance=${encodeURIComponent(instance)}` : ""}` : null);
  const go = (q: Record<string, string>) => {
    window.location.hash = href(["debug"], Object.fromEntries(Object.entries(q).filter(([, v]) => v)));
  };
  const disabled = capture.error instanceof HttpError && capture.error.code === "debug_disabled";
  return (
    <>
      <PageHead title="Live debug" sub="The last raw response of a job instance, side by side with the samples it produced. Refreshed after every run.">
        <Select label="Job" value={job} onChange={(v) => go({ job: v })}>
          <option value="">Choose a job</option>
          {(jobs.data ?? []).map((j) => <option key={j.name} value={j.name}>{j.name}</option>)}
        </Select>
        {capture.data && capture.data.instances.length > 1 && (
          <Select label="Instance" value={instance || capture.data.instance} onChange={(v) => go({ job, instance: v })}>
            {capture.data.instances.map((i) => <option key={i} value={i}>{i}</option>)}
          </Select>
        )}
      </PageHead>
      {!job && <div className="muted">Choose a job to see the last response of one of its instances.</div>}
      {disabled && (
        <div className="card">
          <h3>The live debug endpoint is disabled</h3>
          <p className="small">It keeps the last responses in memory, redacted. Enable it in the Settings document and reload:</p>
          <CodeBlock code={"apiVersion: tapyr/v1alpha1\nkind: Settings\nmetadata: { name: default }\nspec:\n  server:\n    debug:\n      enabled: true\n      maxBodyBytes: 1048576"} lang="yaml" />
        </div>
      )}
      {job && capture.error && !disabled && !capture.data && (
        capture.error instanceof HttpError && capture.error.status === 404
          ? <div className="muted">No capture for {job} yet: it appears after the next run.</div>
          : <Failure error={capture.error} />
      )}
      {job && capture.loading && !capture.data && !capture.error && <Loading />}
      {capture.data && <Capture capture={capture.data} />}
    </>
  );
}

function Capture({ capture: c }: { capture: DebugT }) {
  const pages = useMemo(
    () => c.pages.map((p) => {
      try {
        return { ...p, parsed: JSON.parse(p.body) as unknown, ok: true };
      } catch {
        return { ...p, parsed: undefined, ok: false };
      }
    }),
    [c.pages],
  );
  return (
    <div className="stack">
      <div className="row small">
        <Badge state={c.result} />
        <span className="mono">{c.instance}</span>
        <span className="muted" title={absolute(c.time)}>run {c.run_id}, {since(c.time)}</span>
      </div>
      {(c.problem || c.error) && (
        <div className={`banner ${c.result === "failed" ? "bad" : "warn"} small`}>
          <ProblemNote problem={c.problem} detail={c.error} />
        </div>
      )}
      <div className="debug-grid">
        <div className="stack">
          <h2>Response {pages.length > 1 ? `(${pages.length} pages)` : ""}</h2>
          {pages.length === 0 && <div className="muted small">{noResponse(c)}</div>}
          {pages.map((p, i) => (
            <div className="card" key={i}>
              <div className="row small" style={{ marginBottom: 8 }}>
                <strong>Page {i + 1}</strong>
                <Badge state={p.status < 400 ? "ok" : "failing"}>HTTP {p.status}</Badge>
                {p.truncated && <Badge state="degraded">truncated</Badge>}
              </div>
              {Object.keys(p.headers).length > 0 && (
                <dl className="kv small mono" style={{ marginBottom: 10 }}>
                  {Object.entries(p.headers).map(([k, v]) => (<Fragment key={k}><dt>{k}</dt><dd>{v}</dd></Fragment>))}
                </dl>
              )}
              {p.ok ? (
                <div className="json-wrap">
                  <div className="json"><JsonTree value={p.parsed} open={3} /></div>
                  <CopyButton text={p.body} />
                </div>
              ) : (
                <CodeBlock code={p.body} />
              )}
            </div>
          ))}
        </div>
        <div className="stack">
          <h2>Samples ({c.samples.length})</h2>
          <Table<DebugSample>
            rows={c.samples}
            rowKey={(s) => s.name + JSON.stringify(s.labels)}
            initialSort={{ key: "name" }}
            empty={noSamples(c)}
            columns={[
              { key: "name", label: "Metric", sort: (s) => s.name, render: (s) => <span className="mono small">{s.name}</span> },
              { key: "value", label: "Value", num: true, sort: (s) => s.value, render: (s) => <span className="num">{s.value}</span> },
              { key: "labels", label: "Labels", render: (s) => <span className="mono small">{Object.entries(s.labels).map(([k, v]) => `${k}="${v}"`).join(", ")}</span> },
            ]}
          />
        </div>
      </div>
    </div>
  );
}

// noResponse says why a capture holds no response: the run was refused
// before any request, or the request got no answer.
export function noResponse(c: DebugT): string {
  if (c.result.startsWith("skipped")) return "No request sent: the run was skipped before it (see above).";
  if (c.result === "success") return "No response: the job made no request.";
  return "No response received: the request got no answer (see the error above).";
}

// noSamples says why a capture holds no sample, from what the run did.
export function noSamples(c: DebugT): string {
  if (c.problem?.class === "mapping") return "No sample: the response could not be turned into metrics (see the error above).";
  if (c.result !== "success" && c.result !== "partial") return "No sample: the run did not get its data (see the error above).";
  return "No sample: the response held no item, or the mapping kept none. The mapping errors, if any, are in the logs.";
}
