import { describe, expect, it } from "vitest";
import { instancesEmpty } from "./Jobs";
import { unplanned } from "./Plan";
import type { Job, Plan } from "../lib/types";

const job = (name: string, patch: Partial<Job> = {}): Job => ({
  name, provider: "p", account: "a", role: "metrics", priority: "normal", implicit: false, desired_interval_seconds: 60,
  effective_interval_seconds: 60, instances: 0, collected: true, outputs: [], labels: {}, ...patch,
});

describe("jobs without instances", () => {
  it("says why a job has no instance", () => {
    expect(instancesEmpty(job("d", { collected: false }))).toBe("No instance: the job is not collected.");
    expect(instancesEmpty(job("n", { parent: "sites", waiting: { reason: "parent", parent: "sites" } }))).toBe("No instance yet: sites has not listed its items.");
    expect(instancesEmpty(job("n", { parent: "sites", waiting: { reason: "parent_failing", parent: "sites", failing_job: "sites" } }))).toBe("No instance yet: sites fails.");
    expect(instancesEmpty(job("d", { parent: "networks", waiting: { reason: "parent_failing", parent: "networks", failing_job: "sites" } }))).toBe("No instance yet: networks waits for sites, which fails.");
    expect(instancesEmpty(job("n", { parent: "sites" }))).toBe("No instance: sites listed no item in its last run.");
  });

  it("lists the collected jobs the plan does not hold yet", () => {
    const plan: Plan = { at: "2026-11-03T12:00:00Z", feasible: true, scopes: [], jobs: [
      { name: "devices[target=eu]", provider: "p", account: "a", priority: "normal", desired_interval_seconds: 60, effective_interval_seconds: 60, stretch: 1, instances: 1, pages_per_run: 1 },
      { name: "sites", provider: "p", account: "a", priority: "normal", desired_interval_seconds: 60, effective_interval_seconds: 60, stretch: 1, instances: 1, pages_per_run: 1 },
    ] };
    const jobs = [
      job("devices", { instances: 1 }),
      job("sites", { instances: 1 }),
      job("networks", { parent: "sites", waiting: { reason: "parent", parent: "sites" } }),
      job("clients", { parent: "sites" }),
      job("ignored", { collected: false }),
    ];
    expect(unplanned(plan, jobs)).toEqual([
      { name: "clients", why: "sites listed no item in its last run." },
      { name: "networks", why: "Waiting for sites to list its items: one instance per item." },
    ]);
  });
});
