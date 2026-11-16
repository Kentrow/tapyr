import type { Budget, Timeline } from "../lib/types";
import { clock, count } from "../lib/format";

// timelineScale is the top of the vertical axis of a quota window: the
// limit when the consumption, now or projected, reaches half of it or
// more; else the consumption rounded up, so that 3 calls of 5,400 do not
// draw a flat line, the limit then being marked as off the scale.
export function timelineScale(max: number, used: number, projected: number): { top: number; limitShown: boolean } {
  const reach = Math.max(used, projected, 1);
  if (max > 0 && reach >= max / 2) return { top: Math.max(max, reach) * 1.08, limitShown: true };
  return { top: niceCeil(reach * 1.2), limitShown: max <= 0 };
}

// niceCeil rounds up to 1, 2 or 5 times a power of ten.
export function niceCeil(v: number): number {
  const p = 10 ** Math.floor(Math.log10(Math.max(v, 1)));
  return [1, 2, 5, 10].map((m) => m * p).find((c) => c >= v) ?? 10 * p;
}

// QuotaTimeline draws the consumption of a quota window: cumulative calls
// per bucket, the projection to the window end at the current pace, the
// usable budget and the reserve.
export function QuotaTimeline({ timeline, budget, now, width = 520, height = 170 }: {
  timeline: Timeline;
  budget: Budget;
  now: number;
  width?: number;
  height?: number;
}) {
  const start = Date.parse(timeline.current.start);
  const end = Date.parse(timeline.current.end);
  const pad = { l: 44, r: 10, t: 10, b: 22 };
  const w = width - pad.l - pad.r;
  const h = height - pad.t - pad.b;
  // The cumulative calls, flat until a bucket has calls: a few calls in
  // the last minutes do not draw a slope from the start of the window.
  let total = 0;
  const points: [number, number][] = [[start, 0]];
  for (const b of timeline.current.buckets) {
    const from = Math.max(start, Date.parse(b.start));
    const t = from + timeline.bucket_seconds * 1000;
    points.push([Math.min(from, end), total]);
    total += b.count;
    points.push([Math.min(t, end), total]);
  }
  const usedNow = Math.max(total, budget.used ?? 0);
  const elapsed = Math.max(1, Math.min(now, end) - start);
  const projected = now < end ? usedNow + (usedNow / elapsed) * (end - now) : usedNow;
  const max = budget.max ?? 0;
  const usable = budget.usable ?? max;
  const { top, limitShown } = timelineScale(max, usedNow, projected);
  const x = (t: number) => pad.l + ((t - start) / Math.max(1, end - start)) * w;
  const y = (v: number) => pad.t + h - (v / top) * h;
  const path = points.map(([t, v], i) => `${i ? "L" : "M"}${x(t).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
  const lastT = points[points.length - 1]?.[0] ?? start;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => start + f * (end - start));
  const over = projected > usable;
  const label = `Calls in the window: ${usedNow} used, ${Math.round(projected)} projected at the window end${over ? " (over budget)" : ""}, budget ${usable}${max !== usable ? ` of ${max}` : ""}`;
  return (
    <>
      <svg className="chart" viewBox={`0 0 ${width} ${height}`} width="100%" role="img" aria-label={label}>
        <title>{label}</title>
        {max > 0 && !limitShown && (
          <text x={pad.l + w - 4} y={pad.t + 8} textAnchor="end">limit {count(max)}, above the scale</text>
        )}
        {max > 0 && limitShown && (
          <>
            <rect x={pad.l} y={y(max)} width={w} height={Math.max(0, y(usable) - y(max))} fill="var(--sun-soft)" />
            <line x1={pad.l} x2={pad.l + w} y1={y(max)} y2={y(max)} stroke="var(--red)" strokeDasharray="4 4" />
            <text x={pad.l + 4} y={y(max) - 3}>limit {count(max)}</text>
            {usable !== max && <text x={pad.l + 4} y={y(usable) + 11}>reserve</text>}
          </>
        )}
        <line x1={pad.l} x2={pad.l} y1={pad.t} y2={pad.t + h} stroke="var(--line)" />
        <line x1={pad.l} x2={pad.l + w} y1={pad.t + h} y2={pad.t + h} stroke="var(--line)" />
        {ticks.map((t, i) => (
          // The first and last labels align on the ends of the axis, so that
          // they are not cut.
          <text key={t} x={x(t)} y={height - 6} textAnchor={i === 0 ? "start" : i === ticks.length - 1 ? "end" : "middle"}>
            {clock(new Date(t).toISOString())}
          </text>
        ))}
        <text x={pad.l - 6} y={pad.t + 8} textAnchor="end">{count(Math.round(top))}</text>
        <text x={pad.l - 6} y={pad.t + h} textAnchor="end">0</text>
        <path d={`${path} L${x(lastT)},${y(0)} L${x(start)},${y(0)} Z`} fill="var(--cyan)" opacity="0.15" />
        <path d={path} fill="none" stroke="var(--cyan-2)" strokeWidth="2" />
        {now < end && (
          <line x1={x(Math.max(lastT, Math.min(now, end)))} y1={y(usedNow)} x2={x(end)} y2={y(projected)} stroke={over ? "var(--red)" : "var(--cyan-2)"} strokeWidth="2" strokeDasharray="5 4" />
        )}
        {now < end && over && (
          <text x={pad.l + w - 4} y={Math.max(pad.t + 10, Math.min(pad.t + h - 4, y(projected) + 12))} textAnchor="end" style={{ fill: "var(--red)", fontWeight: 600 }}>
            projected over budget
          </text>
        )}
        {now < end && <line x1={x(now)} x2={x(now)} y1={pad.t} y2={pad.t + h} stroke="var(--ink-2)" strokeDasharray="2 3" opacity="0.6" />}
      </svg>
      <div className="legend">
        <span><i style={{ background: "var(--cyan-2)" }} />consumed</span>
        <span><i style={{ background: "var(--cyan-2)", opacity: 0.5 }} />projection at the current pace (dashed)</span>
        {max > 0 && limitShown && <span><i style={{ background: "var(--red)" }} />limit</span>}
        {max > 0 && limitShown && usable !== max && <span><i style={{ background: "var(--sun-soft)" }} />retry reserve</span>}
      </div>
    </>
  );
}
