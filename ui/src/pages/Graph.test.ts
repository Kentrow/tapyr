import { describe, expect, it } from "vitest";
import { initialViewport, jobAccounts, layout, layoutBounds, nodeWord, readableZoom, selectionAfter, tileHint, worst } from "./Graph";
import type { Graph } from "../lib/types";

const n = (id: string, kind: string) => ({ id, kind, label: id, health: "ok" as const });

function sample(jobs: number): Graph {
  const nodes = [n("p", "provider"), n("a1", "account"), n("a2", "account"), n("t1", "target"), n("o", "output")];
  const edges = [{ from: "p", to: "a1" }, { from: "p", to: "a2" }, { from: "a1", to: "t1" }];
  for (let i = 0; i < jobs; i++) {
    const id = `j${i}`;
    nodes.push(n(id, "job"));
    edges.push({ from: i % 2 ? "a2" : "a1", to: id }, { from: id, to: "o" });
  }
  nodes.push(n("child", "job"));
  edges.push({ from: "j0", to: "child" }, { from: "child", to: "o" });
  return { nodes, edges };
}

describe("graph layout", () => {
  it("groups the jobs of each account and collapses their wires", () => {
    const { nodes, edges } = layout(sample(4), { compact: false });
    const g1 = nodes.find((x) => x.id === "group:a1")!;
    expect(nodes.filter((x) => x.parentId === "group:a1").map((x) => x.id).sort()).toEqual(["child", "j0", "j2"]);
    expect(nodes.filter((x) => x.parentId === "group:a2").map((x) => x.id).sort()).toEqual(["j1", "j3"]);
    // One wire per account and per group to the output; the fan-out child
    // follows its parent and names it instead of being wired to it.
    const ids = edges.map((e) => e.id).sort();
    expect(ids).toEqual(["a1->group:a1", "a1->t1", "a2->group:a2", "group:a1->o", "group:a2->o", "p->a1", "p->a2"].sort());
    const tiles = nodes.filter((x) => x.parentId === "group:a1").map((x) => x.id);
    expect(tiles.indexOf("child")).toBe(tiles.indexOf("j0") + 1);
    expect((nodes.find((x) => x.id === "child")!.data as { parent?: string }).parent).toBe("j0");
    // The account sits in front of its group, the provider between its accounts.
    const a1 = nodes.find((x) => x.id === "a1")!;
    expect(a1.position.x).toBeLessThan(g1.position.x);
    const p = nodes.find((x) => x.id === "p")!;
    const a2 = nodes.find((x) => x.id === "a2")!;
    expect(p.position.y).toBeGreaterThan(a1.position.y);
    expect(p.position.y).toBeLessThan(a2.position.y);
  });

  it("lays out large groups as grids, denser in compact mode", () => {
    const height = (compact: boolean) => Number(layout(sample(80), { compact }).nodes.find((x) => x.id === "group:a1")!.style!.height);
    const columns = (compact: boolean) => new Set(layout(sample(80), { compact }).nodes.filter((x) => x.parentId === "group:a1").map((x) => x.position.x)).size;
    expect(columns(false)).toBe(4);
    expect(columns(true)).toBe(6);
    expect(height(true)).toBeLessThan(height(false) * 0.6);
  });

  it("in compact mode, puts the account groups of a provider in its section, as a grid", () => {
    const nodes = [n("p", "provider"), n("o", "output")];
    const edges: Graph["edges"] = [];
    for (let a = 0; a < 9; a++) {
      nodes.push(n(`a${a}`, "account"));
      edges.push({ from: "p", to: `a${a}` });
      for (let j = 0; j < 3; j++) {
        nodes.push(n(`a${a}j${j}`, "job"));
        edges.push({ from: `a${a}`, to: `a${a}j${j}` }, { from: `a${a}j${j}`, to: "o" });
      }
    }
    const out = layout({ nodes, edges }, { compact: true });
    expect(out.nodes.some((x) => x.id === "a0")).toBe(false);
    // The provider is a section holding its account groups: no wire to them,
    // one wire from the section to the output.
    expect(out.nodes.filter((x) => x.parentId === "section:p")).toHaveLength(9);
    expect(out.edges.map((e) => e.id)).toEqual(["section:p->o"]);
    const columns = new Set(out.nodes.filter((x) => x.parentId === "section:p").map((x) => x.position.x));
    expect(columns.size).toBe(3);
  });

  it("keeps only the filtered nodes", () => {
    const { nodes } = layout(sample(4), { compact: false, keep: (x) => x.id !== "a2" && x.id !== "j1" && x.id !== "j3" });
    expect(nodes.some((x) => x.id === "group:a2")).toBe(false);
    expect(nodes.some((x) => x.id === "group:a1")).toBe(true);
  });

  it("maps each job to its account, through its fan-out parents", () => {
    const accounts = jobAccounts(sample(2));
    expect(accounts.get("j0")).toBe("a1");
    expect(accounts.get("j1")).toBe("a2");
    expect(accounts.get("child")).toBe("a1");
  });

  it("marks the selected node for React Flow", () => {
    const { nodes } = layout(sample(2), { compact: false, selected: "j0" });
    expect(nodes.filter((x) => x.selected).map((x) => x.id)).toEqual(["j0"]);
    expect(nodes.find((x) => x.id === "j0")?.ariaLabel).toBe("job j0, ok");
  });
});

describe("graph selection", () => {
  const select = (id: string, selected: boolean) => ({ type: "select" as const, id, selected });
  it.each([
    { name: "a click or Enter on a node selects it", changes: [select("a", true)], current: null, want: "a" },
    { name: "selecting another node replaces the selection", changes: [select("a", false), select("b", true)], current: "a", want: "b" },
    { name: "in any order", changes: [select("b", true), select("a", false)], current: "a", want: "b" },
    { name: "Escape on the selected node clears it", changes: [select("a", false)], current: "a", want: null },
    { name: "unselecting another node keeps it", changes: [select("b", false)], current: "a", want: "a" },
    { name: "other changes keep it", changes: [{ type: "dimensions" as const, id: "a" }], current: "a", want: "a" },
    { name: "no change keeps it", changes: [], current: null, want: null },
  ])("$name", ({ changes, current, want }) => {
    expect(selectionAfter(changes, current)).toBe(want);
  });
});

describe("waiting jobs", () => {
  it("leaves the jobs that have not run out of the health of their group", () => {
    expect(worst(["ok", "unknown"])).toBe("ok");
    expect(worst(["unknown", "unknown"])).toBe("unknown");
    expect(worst(["ok", "failing", "unknown"])).toBe("failing");
    expect(worst(["ok", "degraded"])).toBe("degraded");
  });
  it("says what a job node waits for", () => {
    expect(nodeWord({ id: "job/n", kind: "job", label: "n", health: "unknown", waiting: "parent" })).toBe("waiting for its parent");
    expect(nodeWord({ id: "account/a", kind: "account", label: "a", health: "unknown" })).toBe("no run yet");
    expect(nodeWord({ id: "job/d", kind: "job", label: "d", health: "failing" })).toBe("failing");
  });
});

describe("graph readability", () => {
  const pane = { width: 1100, height: 700 };
  it("opens a small graph whole, centered, never enlarged", () => {
    const v = initialViewport({ x: 0, y: 0, width: 600, height: 200 }, pane);
    expect(v.zoom).toBe(1);
    expect(v.x).toBe(250);
    expect(v.y).toBe(250);
  });
  it("fits a graph that reads at the readable zoom or more", () => {
    const v = initialViewport({ x: 0, y: 0, width: 1170, height: 400 }, pane);
    expect(v.zoom).toBeCloseTo(1052 / 1170);
    expect(v.zoom).toBeGreaterThanOrEqual(readableZoom);
  });
  it("opens a large graph at the readable zoom on its top left corner", () => {
    const v = initialViewport({ x: -10, y: 20, width: 2400, height: 3000 }, pane);
    expect(v).toEqual({ x: 24 + 10 * readableZoom, y: 24 - 20 * readableZoom, zoom: readableZoom });
    // Wide but short: from its start across, centered down.
    const w = initialViewport({ x: 0, y: 0, width: 2400, height: 300 }, pane);
    expect(w.x).toBe(24);
    expect(w.y).toBe((700 - 300 * readableZoom) / 2);
  });
  it("bounds the layout from the sizes it sets", () => {
    const { nodes } = layout(sample(4), { compact: false });
    const b = layoutBounds(nodes);
    for (const node of nodes.filter((x) => !x.parentId)) {
      expect(node.position.x).toBeGreaterThanOrEqual(b.x);
      expect(node.position.y).toBeGreaterThanOrEqual(b.y);
      expect(node.position.x + Number(node.style?.width ?? 0)).toBeLessThanOrEqual(b.x + b.width);
    }
    expect(layoutBounds([])).toEqual({ x: 0, y: 0, width: 1, height: 1 });
  });
  it("leaves out the column of the targets when no account shown has one", () => {
    const g = sample(2);
    const withTargets = layout(g, { compact: false }).nodes.find((x) => x.id === "group:a1")!.position.x;
    const without = layout({ nodes: g.nodes.filter((x) => x.kind !== "target"), edges: g.edges.filter((e) => e.to !== "t1") }, { compact: false }).nodes.find((x) => x.id === "group:a1")!.position.x;
    expect(without).toBeLessThan(withTargets);
  });
  it("names the tiles for people, with the full name on hover", () => {
    const names = (x: { label: string }) => (x.label === "j0" ? { title: "Sites", detail: "listed for child" } : { title: x.label });
    const { nodes } = layout(sample(2), { compact: false, names });
    const j0 = nodes.find((x) => x.id === "j0")!.data as { title?: string; kind?: string };
    expect(j0).toMatchObject({ title: "Sites", kind: "listed for child" });
    expect((nodes.find((x) => x.id === "child")!.data as { parent?: string }).parent).toBe("Sites");
    expect(tileHint({ node: n("a.p-sites", "job"), title: "Sites", kind: "listed for child" })).toBe("Sites, listed for child (job a.p-sites)");
    expect(tileHint({ node: n("child", "job"), title: "child", parent: "Sites" })).toBe("child, one instance per item of Sites");
  });
});
