import { describe, expect, it } from "vitest";
import { noResponse, noSamples } from "./Debug";
import type { Debug } from "../lib/types";

const capture = (patch: Partial<Debug>): Debug => ({
  job: "devices", instance: "devices", run_id: "1", time: "2026-11-03T12:00:00Z", result: "success", pages: [], samples: [], instances: ["devices"], ...patch,
});

describe("Live debug", () => {
  it("says why there is no response", () => {
    expect(noResponse(capture({ result: "skipped_budget" }))).toBe("No request sent: the run was skipped before it (see above).");
    expect(noResponse(capture({ result: "failed" }))).toBe("No response received: the request got no answer (see the error above).");
  });

  it("says why there is no sample, without blaming the mapping for an auth failure", () => {
    const auth = { class: "auth", title: "t", action: "a", docs_url: "u" };
    expect(noSamples(capture({ result: "failed", problem: auth }))).toBe("No sample: the run did not get its data (see the error above).");
    expect(noSamples(capture({ result: "failed", problem: { ...auth, class: "mapping" } }))).toBe("No sample: the response could not be turned into metrics (see the error above).");
    expect(noSamples(capture({}))).toContain("held no item");
  });
});
