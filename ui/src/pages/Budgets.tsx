import { useEffect, useRef } from "react";
import { Badge, Failure, Loading, Meter, PageHead } from "../components/ui";
import { QuotaTimeline } from "../components/QuotaTimeline";
import { absolute, count, duration, plural, scopeAccounts, since } from "../lib/format";
import { useJSON, useList, useNow } from "../lib/live";
import { useRoute } from "../lib/router";
import type { Budget, Timeline } from "../lib/types";

export function Budgets() {
  const budgets = useList<Budget>("/budgets");
  const route = useRoute();
  const focus = route.query.get("limit") && `${route.query.get("limit")}|${route.query.get("scope") ?? ""}`;
  if (budgets.error && !budgets.data) return <Failure error={budgets.error} />;
  if (!budgets.data) return <Loading />;
  const groups = new Map<string, Budget[]>();
  for (const b of budgets.data) {
    const k = b.provider;
    groups.set(k, [...(groups.get(k) ?? []), b]);
  }
  return (
    <>
      <PageHead title="Budgets" sub="Every limit of every provider, per scope: quotas over their window, rates and concurrency now." />
      {[...groups.entries()].map(([provider, list]) => (
        <section className="section" key={provider}>
          <h2>{provider}</h2>
          <div className="grid wide">
            {list.map((b) => {
              const id = `${b.provider}/${b.limit}|${b.scope}`;
              return <BudgetCard key={id} budget={b} focused={focus === id} />;
            })}
          </div>
        </section>
      ))}
    </>
  );
}

function BudgetCard({ budget: b, focused }: { budget: Budget; focused: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (focused) ref.current?.scrollIntoView({ block: "center" });
  }, [focused]);
  return (
    <div className="card" ref={ref} style={focused ? { outline: "3px solid var(--cyan)" } : undefined}>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h3>
          {b.limit} <span className="muted small">{b.kind}</span>
        </h3>
        <BudgetState budget={b} />
      </div>
      <div className="muted small" style={{ marginBottom: 10 }}>
        <span className="mono">{b.scope}</span>
        {(b.accounts ?? []).length > 0 && !b.scope.split(",").some((d) => d.startsWith("account=")) && <>, {plural(b.accounts.length, "account")}: {scopeAccounts(b)}</>}
      </div>
      {b.kind === "quota" ? <QuotaBody budget={b} /> : b.kind === "rate" ? <RateBody budget={b} /> : <ConcurrencyBody budget={b} />}
    </div>
  );
}

function BudgetState({ budget: b }: { budget: Budget }) {
  const now = useNow();
  const penalty = b.penalty_until && Date.parse(b.penalty_until) > now;
  if (penalty) return <Badge state="failing">rate limited until {since(b.penalty_until)}</Badge>;
  if (b.kind === "quota" && b.exhaustion_eta_seconds !== undefined) return <Badge state="failing">exhausted in {duration(b.exhaustion_eta_seconds)}</Badge>;
  if (b.limit_mismatch) return <Badge state="degraded">headers disagree</Badge>;
  return <Badge state="ok">ok</Badge>;
}

function QuotaBody({ budget: b }: { budget: Budget }) {
  const now = useNow();
  const timeline = useJSON<Timeline>(`/budgets/${encodeURIComponent(`${b.provider}/${b.limit}`)}/timeline?scope=${encodeURIComponent(b.scope)}`);
  const max = b.max ?? 0;
  const used = b.used ?? 0;
  const external = b.external_usage ?? 0;
  return (
    <div className="stack" style={{ gap: 12 }}>
      <dl className="kv small">
        <dt>Window</dt>
        <dd>
          {b.rolling ? "rolling, " : ""}
          <span title={absolute(b.window_start)}>from {since(b.window_start)}</span>, <span title={absolute(b.window_end)}>resets {since(b.window_end)}</span>
        </dd>
        <dt>Limit</dt>
        <dd className="num">
          {count(max)}
          {b.usable !== undefined && b.usable !== max && <span className="muted"> ({count(b.usable)} usable after the share)</span>}
        </dd>
        <dt>Used by Tapyr</dt>
        <dd className="num">{count(used)}</dd>
        <dt>Used by others</dt>
        <dd className="num">{b.external_usage !== undefined ? count(external) : <span className="muted">unknown (no quota headers)</span>}</dd>
        <dt>Remaining</dt>
        <dd className="num">
          {count(b.remaining)}
          {b.remaining_observed !== undefined && <span className="muted"> ({count(b.remaining_observed)} reported by the API)</span>}
        </dd>
        <dt>Retry reserve</dt>
        <dd className="num">
          {count(b.reserve_remaining)} of {count(b.reserve)}
        </dd>
      </dl>
      {timeline.data ? (
        <QuotaTimeline timeline={timeline.data} budget={b} now={now} />
      ) : timeline.error ? (
        <div className="muted small">No timeline: {timeline.error.message}</div>
      ) : (
        <Loading />
      )}
      <div className="legend">
        <span><i style={{ background: "var(--cyan-2)" }} />consumed</span>
        <span><i style={{ background: "var(--cyan-2)", opacity: 0.5 }} />projection at the current pace (dashed)</span>
        <span><i style={{ background: "var(--red)" }} />limit</span>
        <span><i style={{ background: "var(--sun-soft)" }} />retry reserve</span>
      </div>
    </div>
  );
}

function RateBody({ budget: b }: { budget: Budget }) {
  const burst = b.burst ?? 0;
  const tokens = b.tokens ?? 0;
  return (
    <div className="stack" style={{ gap: 10 }}>
      <div className="row" style={{ alignItems: "baseline" }}>
        <span className="kpi">
          {b.rate_per_second !== undefined ? +b.rate_per_second.toFixed(3) : "-"}
          <small>requests per second</small>
        </span>
      </div>
      <Meter total={burst} label={`${tokens.toFixed(1)} of ${burst} tokens available`} parts={[{ value: tokens, color: "var(--cyan-2)", label: `${tokens.toFixed(1)} of ${count(burst)} burst tokens available` }]} />
      {b.penalty_until && <div className="small muted">Last 429 penalty until {absolute(b.penalty_until)}</div>}
    </div>
  );
}

function ConcurrencyBody({ budget: b }: { budget: Budget }) {
  const max = b.max ?? 0;
  const inUse = b.in_use ?? 0;
  return (
    <div className="stack" style={{ gap: 10 }}>
      <span className="kpi">
        {inUse}
        <small>of {max} requests in flight</small>
      </span>
      <Meter total={max} label={`${inUse} of ${max} requests in flight`} parts={[{ value: inUse, color: "var(--cyan-2)", label: "in flight" }]} />
    </div>
  );
}
