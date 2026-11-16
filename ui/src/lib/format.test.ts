import { describe, expect, it } from "vitest";
import { count, duration, healthClass, jobLabels, jobTitle, jobWords, limitSummary, limitTitle, outputHealth, plural, scopeAccounts, seriesStatus, since, sourceWords, toYAML, versionLabel, waitingText, waitingWord } from "./format";
import type { Budget, Job, Output } from "./types";

describe("format", () => {
  it("names a limit and says what it allows", () => {
    const b: Budget = { provider: "p", limit: "account-rate", kind: "rate", scope: "account=demo", accounts: ["demo"], rate_per_second: 10 };
    expect(limitTitle("account-rate")).toBe("Account rate");
    expect(limitTitle("daily")).toBe("Daily");
    expect(limitSummary(b)).toBe("10 requests per second");
    expect(limitSummary({ ...b, rate_per_second: 0.5 })).toBe("30 requests per minute");
    expect(limitSummary({ ...b, kind: "concurrency", max: 4 })).toBe("4 requests at once");
    expect(limitSummary({ ...b, kind: "quota", max: 5000, period: "day" })).toBe("5,000 calls per day");
    expect(limitSummary({ ...b, kind: "quota", max: 100, window_start: "2026-11-03T11:00:00Z", window_end: "2026-11-03T12:00:00Z" })).toBe("100 calls per 1h, rolling");
    expect(sourceWords({ kind: "vendor-docs", url: "https://example.com" })).toBe("from the vendor documentation");
    expect(sourceWords({ kind: "measured", date: "2026-11" })).toBe("measured against the real API (2026-11)");
    expect(sourceWords({ kind: "assumed" })).toBe("assumed (the vendor documents no value)");
    expect(sourceWords(undefined)).toBe("no source given by the provider definition");
  });
  it("says what a job that has not run yet waits for", () => {
    const now = Date.parse("2026-11-03T12:00:00Z");
    expect(waitingText({ reason: "first_run", until: "2026-11-03T12:00:40Z" }, now)).toEqual({
      short: "first run in 40s",
      long: "Waiting for its first run, due in 40s.",
    });
    expect(waitingText({ reason: "first_run" }, now).short).toBe("first run soon");
    expect(waitingText({ reason: "parent", parent: "sites", until: "2026-11-03T12:05:00Z" }, now)).toEqual({
      short: "waiting for sites",
      long: "Waiting for sites to list its items: one instance per item. The discovery moves on in 5m.",
    });
    const failing = { reason: "parent_failing" as const, parent: "sites", failing_job: "sites", error: "HTTP 401", failed_at: "2026-11-03T11:58:00Z", until: "2026-11-03T13:00:00Z" };
    expect(waitingText(failing, now)).toEqual({
      short: "waiting: sites fails",
      long: "Waiting for sites, which fails: HTTP 401 (2m ago). Next try in 1h.",
    });
    expect(waitingText({ ...failing, parent: "networks" }, now).long).toBe("Waiting for networks, whose parent sites fails: HTTP 401 (2m ago). Next try in 1h.");
    const problem = { class: "auth", title: "The provider rejected the credentials of lab.", action: "Check it.", docs_url: "u" };
    expect(waitingText({ ...failing, problem }, now).long).toBe("Waiting for sites, which fails (2m ago). Next try in 1h.");
    // Implicit jobs are named by what they list.
    const words = (job: string) => (job === "demo.example-sim-sites" ? "the job listing the sites" : job);
    expect(waitingText({ reason: "parent", parent: "demo.example-sim-sites" }, now, words)).toEqual({
      short: "waiting for the job listing the sites",
      long: "Waiting for the job listing the sites: one instance per item.",
    });
    expect(waitingText({ ...failing, parent: "networks", failing_job: "demo.example-sim-sites" }, now, words)).toMatchObject({
      short: "waiting: the job listing the sites fails",
      long: "Waiting for networks, whose parent the job listing the sites fails: HTTP 401 (2m ago). Next try in 1h.",
    });
    expect(waitingWord("parent_failing")).toBe("waiting, parent failing");
    expect(waitingWord(undefined)).toBe("no run yet");
  });
  it("names implicit jobs by what they list and for whom", () => {
    const base = { provider: "example-sim", account: "demo", role: "both", priority: "low", desired_interval_seconds: 60, effective_interval_seconds: 60, instances: 1, collected: true, outputs: [], labels: {} };
    const nets: Job = { ...base, name: "demo.example-sim-networks", implicit: true, items: "networks", discovery_for: ["network-devices", "routers"] };
    const own: Job = { ...base, name: "demo-sites", implicit: false };
    expect(jobTitle(nets)).toEqual({ title: "Networks", detail: "listed for network-devices, routers" });
    expect(jobTitle({ ...nets, discovery_for: undefined })).toEqual({ title: "Networks", detail: "listed for a fan-out" });
    // A Tapyr without the items (an older server) shows the name.
    expect(jobTitle({ ...nets, items: undefined })).toEqual({ title: "demo.example-sim-networks" });
    expect(jobTitle(own)).toEqual({ title: "demo-sites" });
    const label = jobLabels([nets, own]);
    expect(label("demo.example-sim-networks")).toBe("Networks, listed for network-devices, routers");
    expect(label("demo-sites")).toBe("demo-sites");
    expect(label("Configuration reloaded")).toBe("Configuration reloaded");
    const words = jobWords([nets, own]);
    expect(words("demo.example-sim-networks")).toBe("the job listing the networks");
    expect(words("demo-sites")).toBe("demo-sites");
  });
  it("says how an output stands against its series limit", () => {
    const o = { series: 950, series_refused: 0, samples_refused: 0 };
    expect(seriesStatus(o)).toMatchObject({ state: "none", title: "950 series" });
    expect(seriesStatus({ ...o, max_series: 2000 })).toMatchObject({ state: "ok", title: "950 of 2,000 series" });
    expect(seriesStatus({ ...o, max_series: 1000 })).toMatchObject({ state: "near", title: "Close to the series limit" });
    expect(seriesStatus({ ...o, max_series: 950 })).toMatchObject({ state: "full", title: "At the series limit" });
    const limited = seriesStatus({ series: 1000, max_series: 1000, series_refused: 37, samples_refused: 74 });
    expect(limited.state).toBe("limited");
    expect(limited.detail).toBe(
      "1,000 of 1,000 series: 37 new series not sent to this output (74 samples dropped since the start). The series already sent keep flowing. Raise limits.maxSeries, or drop metrics with processing rules.",
    );
  });
  it("names the accounts of a scope", () => {
    expect(scopeAccounts({ scope: "account=campus", accounts: ["campus"] })).toBe("campus");
    expect(scopeAccounts({ scope: "credential=ab12", accounts: ["fleet-a", "fleet-b"] })).toBe("fleet-a and fleet-b");
    expect(scopeAccounts({ scope: "instance", accounts: ["a", "b", "c"] })).toBe("a, b and c");
    expect(scopeAccounts({ scope: "instance", accounts: ["a", "b", "c", "d"] })).toBe("a, b and 2 more");
    expect(scopeAccounts({ scope: "credential=ab12", accounts: [] })).toBe("credential=ab12");
  });
  it("labels the running version", () => {
    expect(versionLabel({ version: "0.3.0", revision: "3b0e90b", go_version: "go1.27.4" })).toEqual({
      text: "Tapyr v0.3.0",
      title: "Tapyr 0.3.0, revision 3b0e90b, built with go1.27.4",
    });
    expect(versionLabel({ version: "0.4.0-rc.1", revision: "8c95410", go_version: "go1.27.4" }).text).toBe("Tapyr v0.4.0-rc.1");
    // A binary built without a version (go build, make build): a dev build, named by its revision.
    expect(versionLabel({ version: "0.0.0-dev", revision: "995f2e2-dirty", go_version: "go1.27.4" })).toEqual({
      text: "Tapyr dev 995f2e2-dirty",
      title: "Tapyr 0.0.0-dev, revision 995f2e2-dirty, built with go1.27.4",
    });
    expect(versionLabel({ version: "0.0.0-dev", revision: "", go_version: "go1.27.4" })).toEqual({
      text: "Tapyr dev",
      title: "Tapyr 0.0.0-dev, built with go1.27.4",
    });
    // go build in a clone: a Go pseudo-version and the full commit.
    expect(
      versionLabel({
        version: "0.3.1-0.20261113104325-995f2e2f21c0+dirty",
        revision: "995f2e2f21c092118908381e79e6397475a4e734-dirty",
        go_version: "go1.27.4",
      }),
    ).toEqual({
      text: "Tapyr dev 995f2e2-dirty",
      title: "Tapyr 0.3.1-0.20261113104325-995f2e2f21c0+dirty, revision 995f2e2f21c092118908381e79e6397475a4e734-dirty, built with go1.27.4",
    });
  });
  it("formats durations compactly", () => {
    expect(duration(45)).toBe("45s");
    expect(duration(300)).toBe("5m");
    expect(duration(330)).toBe("5m30s");
    expect(duration(5400)).toBe("1h30m");
    expect(duration(90061)).toBe("1d1h");
    expect(duration(undefined)).toBe("-");
  });
  it("formats counts and plurals", () => {
    expect(count(4986)).toBe("4,986");
    expect(count(100000)).toBe("100.0k");
    expect(count(2_500_000)).toBe("2.5M");
    expect(plural(1, "instance")).toBe("1 instance");
    expect(plural(12, "instance")).toBe("12 instances");
  });
  it("shows times relative to now", () => {
    const now = Date.parse("2026-11-03T12:00:00Z");
    expect(since("2026-11-03T11:55:00Z", now)).toBe("5m ago");
    expect(since("2026-11-03T12:01:00Z", now)).toBe("in 1m");
    expect(since(undefined, now)).toBe("never");
  });
  it("renders YAML", () => {
    expect(toYAML({ job: { interval: "5m0s", path: "/a/{b}", outputs: [], n: 3, list: ["x", { k: true }] } }).trimStart()).toBe(
      'job:\n  interval: "5m0s"\n  path: "/a/{b}"\n  outputs: []\n  n: 3\n  list:\n    - x\n    - k: true',
    );
  });
  it("maps states to classes", () => {
    expect(healthClass("success")).toBe("ok");
    expect(healthClass("skipped_budget")).toBe("warn");
    expect(healthClass("open")).toBe("bad");
    expect(healthClass("whatever")).toBe("idle");
  });
});

describe("output health", () => {
  const push: Output = {
    name: "vm", type: "prometheus_remote_write", queue: 0, capacity: 100, sent: 0, dropped: 0,
    series: 0, series_refused: 0, samples_refused: 0, unmeasured_jobs: [],
  };
  it.each([
    { name: "a scraped output is always ok", o: { ...push, type: "prometheus", last_error: "x", last_error_at: "2026-11-03T12:00:00Z" }, want: "ok" },
    { name: "nothing sent yet is unknown", o: push, want: "unknown" },
    { name: "a success is ok", o: { ...push, last_success: "2026-11-03T12:00:00Z" }, want: "ok" },
    { name: "an error without a success is failing", o: { ...push, last_error: "refused", last_error_at: "2026-11-03T12:00:00Z" }, want: "failing" },
    { name: "an error after the last success is failing", o: { ...push, last_success: "2026-11-03T11:00:00Z", last_error: "refused", last_error_at: "2026-11-03T12:00:00Z" }, want: "failing" },
    { name: "a success after the last error is ok", o: { ...push, last_success: "2026-11-03T12:00:00Z", last_error: "refused", last_error_at: "2026-11-03T11:00:00Z" }, want: "ok" },
  ])("$name", ({ o, want }) => {
    expect(outputHealth(o)).toBe(want);
  });
});
