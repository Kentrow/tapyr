import { describe, expect, it } from "vitest";
import { niceCeil, timelineScale } from "./QuotaTimeline";

describe("quota timeline scale", () => {
  it("scales to the consumption when it is far below the limit", () => {
    // 3 calls early in a window of 5,400: no flat line at the bottom.
    expect(timelineScale(5400, 3, 40)).toEqual({ top: 50, limitShown: false });
  });
  it("shows the limit once the consumption reaches half of it", () => {
    expect(timelineScale(5400, 2000, 2700)).toEqual({ top: 5400 * 1.08, limitShown: true });
    expect(timelineScale(5400, 4000, 6000)).toEqual({ top: 6000 * 1.08, limitShown: true });
  });
  it("has no limit to mark without a maximum", () => {
    expect(timelineScale(0, 3, 3)).toEqual({ top: 5, limitShown: true });
  });
  it("rounds up to 1, 2 or 5 times a power of ten", () => {
    expect([0.5, 1, 3.6, 48, 120, 7000].map(niceCeil)).toEqual([1, 1, 5, 50, 200, 10000]);
  });
});
