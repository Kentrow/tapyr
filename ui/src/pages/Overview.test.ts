import { describe, expect, it } from "vitest";
import { jobCounts, planHint, quotaHint, squareWord, unhealthyAccounts, uptime } from "./Overview";
import type { Account, Budget, Job } from "../lib/types";

const now = Date.parse("2026-11-03T12:00:00Z");
const quota: Budget = { provider: "meraki", limit: "daily", kind: "quota", scope: "account", accounts: [], window_end: "2026-11-03T18:00:00Z" };

describe("tightest quota hint", () => {
  it("says when the quota resets", () => {
    expect(quotaHint(quota, now)).toBe("meraki / daily, resets in 6h");
  });
  it("says when the quota runs out before its reset", () => {
    expect(quotaHint({ ...quota, exhaustion_eta_seconds: 5400 }, now)).toBe("meraki / daily, runs out in 1h30m at the current pace, resets in 6h");
  });
});

describe("plan hint", () => {
  it("names the quota that would run out with the planned intervals", () => {
    const scope = { limit: "example-sim/daily", scope: "account=lab", period_seconds: 86400, budget: 4500, reserve: 500, projected: 9000, feasible: false, exhaustion_at: "2026-11-03T14:00:00Z", jobs: [] };
    const plan = { at: "2026-11-03T12:00:00Z", feasible: false, jobs: [], scopes: [{ ...scope, feasible: true, limit: "other" }, scope] };
    expect(planHint(plan, false, now)).toBe("the daily quota of account lab would run out in 2h with the planned intervals");
    expect(planHint(undefined, false, now)).toBe("a quota would run out before its reset");
    expect(planHint(plan, true, now)).toBe("every critical job on schedule");
  });
});

describe("unhealthy accounts", () => {
  const account = (name: string, health: Account["health"], reasons: string[], reauth = false): Account => ({
    name, provider: "p", labels: {}, targets: [], auth: { type: "api_key", reauth_required: reauth }, health, health_reasons: reasons,
  });
  it("lists failing accounts first, then degraded ones, with their reasons", () => {
    const got = unhealthyAccounts([
      account("ok", "ok", []),
      account("b", "degraded", ["jobs: server /sites: HTTP 502"]),
      account("a", "failing", ["devices: auth /devices: HTTP 401"]),
      account("new", "unknown", []),
    ]);
    expect(got.map((a) => [a.name, a.health, a.reasons])).toEqual([
      ["a", "failing", ["devices: auth /devices: HTTP 401"]],
      ["b", "degraded", ["jobs: server /sites: HTTP 502"]],
    ]);
  });
  it("keeps the problem of an account, but not a re-bootstrap, which has its own cell", () => {
    const problem = { class: "auth", title: "The provider rejected the credentials of a.", action: "Check it.", docs_url: "u" };
    const got = unhealthyAccounts([
      { ...account("a", "failing", ["devices: auth /sites: HTTP 401"]), problem },
      { ...account("b", "failing", ["circuit open"], true), problem: { ...problem, class: "reauth" } },
    ]);
    expect(got.map((a) => [a.name, a.problem?.class])).toEqual([["a", "auth"], ["b", undefined]]);
  });

  it("leaves a re-bootstrap to its own cell", () => {
    const got = unhealthyAccounts([
      account("only", "failing", ["aps: auth /aps: a manual re-bootstrap is required", "the credentials need a manual re-bootstrap"], true),
      account("more", "failing", ["the credentials need a manual re-bootstrap", "circuit open"], true),
    ]);
    expect(got.map((a) => [a.name, a.reasons])).toEqual([["more", ["circuit open"]]]);
  });
});

describe("jobs of a provider", () => {
  const job = (name: string, patch: Partial<Job> = {}): Job => ({
    name, provider: "p", account: "a", role: "metrics", priority: "normal", implicit: false, desired_interval_seconds: 60,
    effective_interval_seconds: 60, instances: 1, collected: true, outputs: [], labels: {}, ...patch,
  });
  const run = (result: string) => ({ run_id: "r", instance: "i", result, started_at: "2026-11-03T11:59:00Z", ended_at: "2026-11-03T11:59:01Z" });
  const now = Date.parse("2026-11-03T12:00:00Z");

  it("tells waiting from failing", () => {
    const first = job("devices", { waiting: { reason: "first_run", until: "2026-11-03T12:00:30Z" } });
    expect(jobCounts([job("sites", { last_run: run("success") }), first, job("clients", { waiting: { reason: "first_run" } })])).toBe("3 jobs: 1 succeeded, 2 waiting for their first run");
    expect(jobCounts([first])).toBe("1 job: 0 succeeded, 1 waiting for its first run");
    const child = job("networks", { instances: 0, waiting: { reason: "parent_failing", parent: "sites", failing_job: "sites" } });
    expect(jobCounts([job("sites", { last_run: run("failed") }), child])).toBe("2 jobs: 0 succeeded, 1 failing, 1 waiting for a failing parent");
    expect(jobCounts([first, child])).toBe("2 jobs: 0 succeeded, 2 waiting");
    expect(squareWord(first, now)).toBe("first run in 30s");
    expect(squareWord(child, now)).toBe("waiting: sites fails");
    expect(squareWord(job("sites", { last_run: run("failed") }), now)).toBe("failed");
  });
});

describe("uptime", () => {
  it("moves with the clock between two loads of the status", () => {
    const s = { started_at: "2026-11-03T12:00:00Z", uptime_seconds: 60 };
    expect(uptime(s, Date.parse("2026-11-03T12:05:00Z"))).toBe(300);
    expect(uptime({ ...s, started_at: "" }, Date.parse("2026-11-03T12:05:00Z"))).toBe(60);
  });
});
