import { describe, expect, it } from "vitest";
import { quotaHint, unhealthyAccounts } from "./Overview";
import type { Account, Budget } from "../lib/types";

const now = Date.parse("2026-11-03T12:00:00Z");
const quota: Budget = { provider: "meraki", limit: "daily", kind: "quota", scope: "account", window_end: "2026-11-03T18:00:00Z" };

describe("tightest quota hint", () => {
  it("says when the quota resets", () => {
    expect(quotaHint(quota, now)).toBe("meraki / daily, resets in 6h");
  });
  it("says when the quota runs out before its reset", () => {
    expect(quotaHint({ ...quota, exhaustion_eta_seconds: 5400 }, now)).toBe("meraki / daily, runs out in 1h30m, resets in 6h");
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
  it("leaves a re-bootstrap to its own cell", () => {
    const got = unhealthyAccounts([
      account("only", "failing", ["aps: auth /aps: a manual re-bootstrap is required", "the credentials need a manual re-bootstrap"], true),
      account("more", "failing", ["the credentials need a manual re-bootstrap", "circuit open"], true),
    ]);
    expect(got.map((a) => [a.name, a.reasons])).toEqual([["more", ["circuit open"]]]);
  });
});
