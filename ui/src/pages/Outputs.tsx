import { useEffect, useRef, useState } from "react";
import { Badge, Failure, Loading, Meter, PageHead, Sparkline } from "../components/ui";
import { Icon } from "../components/Icon";
import { absolute, count, outputHealth, seriesStatus, since } from "../lib/format";
import { useList } from "../lib/live";
import type { Output } from "../lib/types";

// The send rate of an output is the difference of its sent counter between
// two refreshes, kept for the last 30 refreshes.
const history = 30;

export function Outputs() {
  const outputs = useList<Output>("/outputs");
  const prev = useRef(new Map<string, { sent: number; at: number }>());
  const [rates, setRates] = useState<Record<string, number[]>>({});
  useEffect(() => {
    if (!outputs.data) return;
    const now = Date.now();
    setRates((r) => {
      const next = { ...r };
      for (const o of outputs.data!) {
        const p = prev.current.get(o.name);
        if (p && now > p.at) {
          const rate = Math.max(0, (o.sent - p.sent) / ((now - p.at) / 1000));
          next[o.name] = [...(next[o.name] ?? []), rate].slice(-history);
        }
        prev.current.set(o.name, { sent: o.sent, at: now });
      }
      return next;
    });
  }, [outputs.data]);
  if (outputs.error && !outputs.data) return <Failure error={outputs.error} />;
  if (!outputs.data) return <Loading />;
  return (
    <>
      <PageHead title="Outputs" sub="Where the samples go: the series each output receives and its limit, queue fill, what was sent and dropped, and the last errors." />
      <div className="grid wide">
        {outputs.data.map((o) => {
          const health = outputHealth(o);
          const values = rates[o.name] ?? [];
          const series = seriesStatus(o);
          return (
            <div className="card" key={o.name}>
              <div className="row" style={{ justifyContent: "space-between" }}>
                <h3>
                  {o.name} <span className="muted small">{o.type}</span>
                </h3>
                <span className="row" style={{ gap: 8 }}>
                  {series.state === "limited" && <Badge state="degraded">series limit reached</Badge>}
                  <Badge state={health}>{health === "failing" ? "failing" : health === "unknown" ? "not sent yet" : o.type === "prometheus" ? "serving" : "sending"}</Badge>
                </span>
              </div>
              <div className="series-block">
                <div className="label">Series</div>
                {o.max_series ? (
                  <Meter total={o.max_series} label={`${o.series} of ${o.max_series} series`} parts={[{
                    value: Math.min(o.series, o.max_series),
                    color: series.state === "ok" ? "var(--cyan-2)" : "var(--sun)",
                    label: `${count(o.series)} of ${count(o.max_series)} series`,
                  }]} />
                ) : null}
                <div className={`series-status ${series.state}`}>
                  {series.state !== "ok" && series.state !== "none" && <Icon name="alert" />}
                  <div>
                    <strong>{series.title}</strong>
                    <div className="small">{series.detail}</div>
                  </div>
                </div>
                {o.unmeasured_jobs.length > 0 && (
                  <div className="small muted">
                    Not counted yet (no successful run): {o.unmeasured_jobs.join(", ")}.
                  </div>
                )}
              </div>
              {o.capacity > 0 && <div className="label" style={{ marginTop: 14 }}>Queue</div>}
              {o.capacity > 0 ? (
                <Meter total={o.capacity} label={`${o.queue} of ${o.capacity} samples queued`} parts={[{ value: o.queue, color: o.queue > o.capacity * 0.8 ? "var(--red)" : "var(--cyan-2)", label: `${count(o.queue)} of ${count(o.capacity)} samples queued` }]} />
              ) : (
                <div className="small muted">Served on /metrics (pull): no queue.</div>
              )}
              {o.type !== "prometheus" && <dl className="kv small" style={{ marginTop: 12 }}>
                <dt>Sent</dt><dd className="num">{count(o.sent)}</dd>
                <dt>Dropped</dt><dd className="num">{count(o.dropped)}</dd>
                <dt>Send rate</dt>
                <dd className="row" style={{ gap: 8 }}>
                  <Sparkline values={values} label={`send rate of ${o.name}`} />
                  <span className="num">{values.length ? `${values[values.length - 1]!.toFixed(1)}/s` : "-"}</span>
                </dd>
                <dt>Last success</dt><dd title={absolute(o.last_success)}>{since(o.last_success)}</dd>
                {o.last_error && (<><dt>Last error</dt><dd className="mono small" title={absolute(o.last_error_at)}>{o.last_error} ({since(o.last_error_at)})</dd></>)}
              </dl>}
            </div>
          );
        })}
      </div>
    </>
  );
}
