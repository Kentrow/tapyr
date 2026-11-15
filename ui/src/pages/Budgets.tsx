import { useEffect, useRef } from "react";
import { Badge, Failure, Loading, Meter, PageHead } from "../components/ui";
import { QuotaTimeline } from "../components/QuotaTimeline";
import { absolute, count, duration, limitSummary, limitTitle, plural, scopeAccounts, scopeWords, since, sourceWords } from "../lib/format";
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
      <PageHead title="Budgets" sub="Every limit of every provider, per scope: quotas over their window, rates and concurrency now. The limits come from the provider definitions, each with its source." />
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
      <div className="row" style={{ justifyContent: "space-between", alignItems: "flex-start" }}>
        <div>
          <h3 style={{ marginBottom: 2 }} title={`${b.provider}/${b.limit} (${b.kind}) in the provider definition`}>{limitTitle(b.limit)}</h3>
          <div className="small">
            {limitSummary(b)}, <span title={b.scope}>for {scopeWords(b.scope)}</span>
            {(b.accounts ?? []).length > 0 && !b.scope.split(",").some((d) => d.startsWith("account=")) && <span className="muted"> ({plural(b.accounts.length, "account")}: {scopeAccounts(b)})</span>}
          </div>
        </div>
        <BudgetState budget={b} />
      </div>
      <div className="limit-about small muted">
        {b.description && <div>{b.description}</div>}
        <div>
          Source: {b.source?.url ? <a href={b.source.url} target="_blank" rel="noreferrer">{sourceWords(b.source)}</a> : sourceWords(b.source)}
        </div>
      </div>
      {b.kind === "quota" ? <QuotaBody budget={b} /> : b.kind === "rate" ? <RateBody budget={b} /> : <ConcurrencyBody budget={b} />}
    </div>
  );
}

function BudgetState({ budget: b }: { budget: Budget }) {
  const now = useNow();
  const penalty = b.penalty_until && Date.parse(b.penalty_until) > now;
  if (penalty) return <Badge state="failing">rate limited until {since(b.penalty_until)}</Badge>;
  if (b.kind === "quota" && b.exhaustion_eta_seconds !== undefined) return <span title="At the pace of the consumption so far, others included"><Badge state="failing">runs out in {duration(b.exhaustion_eta_seconds)}</Badge></span>;
  if (b.limit_mismatch) return <span title="The quota headers announce another maximum than the provider definition: Tapyr follows the headers."><Badge state="degraded">the API reports another maximum</Badge></span>;
  return <Badge state="ok">ok</Badge>;
}

// help explains the figures of a quota card, for the people who do not
// know the budget engine.
export const help = {
  limit: "The maximum of the window: declared by the provider definition, or learned from the quota headers of the responses.",
  used: "The calls Tapyr made in this window.",
  others: "The calls of other clients of the same quota (the vendor UI, scripts, other integrations): what the provider counts, minus what Tapyr made. Known only when the API sends quota headers.",
  remaining: "What is left in this window, as Tapyr counts it.",
  observed: "What is left as the API reports it in its quota headers. Tapyr spends no more than the lower of both.",
  reserve: "Calls kept for retries and critical jobs: the planner leaves them out, so that a retry never finds the quota empty.",
};

// Help marks a term that has an explanation: dotted underline, the text as
// a tooltip and for screen readers.
function Help({ text, children }: { text: string; children: React.ReactNode }) {
  return <abbr className="help" title={text}>{children}</abbr>;
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
        <dt><Help text={help.limit}>Limit</Help></dt>
        <dd className="num">
          {count(max)}
          {b.usable !== undefined && b.usable !== max && <span className="muted"> ({count(b.usable)} usable after the share)</span>}
        </dd>
        <dt><Help text={help.used}>Used by Tapyr</Help></dt>
        <dd className="num">{count(used)}</dd>
        <dt><Help text={help.others}>Used by others</Help></dt>
        <dd className="num">{b.external_usage !== undefined ? count(external) : <span className="muted">unknown (no quota headers)</span>}</dd>
        <dt><Help text={help.remaining}>Remaining</Help></dt>
        <dd className="num">
          {count(b.remaining)}
          {b.remaining_observed !== undefined && <span className="muted"> (<Help text={help.observed}>{count(b.remaining_observed)} reported by the API</Help>)</span>}
        </dd>
        <dt><Help text={help.reserve}>Retry reserve</Help></dt>
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
