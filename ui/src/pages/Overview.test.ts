import { describe, expect, it } from "vitest";
import { quotaHint } from "./Overview";
import type { Budget } from "../lib/types";

const now = Date.parse("2026-11-03T12:00:00Z");
const quota: Budget = { provider: "meraki", limit: "daily", kind: "quota", scope: "account", accounts: [], window_end: "2026-11-03T18:00:00Z" };

describe("tightest quota hint", () => {
  it("says when the quota resets", () => {
    expect(quotaHint(quota, now)).toBe("meraki / daily, resets in 6h");
  });
  it("says when the quota runs out before its reset", () => {
    expect(quotaHint({ ...quota, exhaustion_eta_seconds: 5400 }, now)).toBe("meraki / daily, runs out in 1h30m, resets in 6h");
  });
});
