import { useMemo, useState } from "react";
import { Background, Controls, Handle, Position, ReactFlow, type Edge, type Node, type NodeChange, type NodeProps } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { Badge, Failure, Loading, PageHead, Select, StateMark, useColorMode } from "../components/ui";
import { plural } from "../lib/format";
import { useJSON, useList } from "../lib/live";
import { href } from "../lib/router";
import type { Account, Graph as GraphT, GraphNode, Health } from "../lib/types";

// The graph shows providers, accounts and their targets on the left, the
// jobs of each account grouped in a box laid out as a grid (there are many
// more jobs than anything else), and the outputs on the right. One wire
// links an account to its box and a box to the outputs its jobs feed;
// inside a box, wires link fan-out parents to their children.

type TileData = { node: GraphNode; selected: boolean; dim: boolean; compact: boolean; parent?: string };
type GroupData = { account: string; health: Health; jobs: number; targets: number; selected: boolean; section?: boolean };
type TileNode = Node<TileData, "tile">;
type GroupNode = Node<GroupData, "group">;

function Tile({ data }: NodeProps<TileNode>) {
  const n = data.node;
  return (
    <div
      className={`tnode-box ${n.health}${data.selected ? " selected" : ""}${data.compact ? " compact" : ""}`}
      style={{ opacity: data.dim ? 0.25 : 1 }}
      title={data.parent ? `${n.label}, one instance per item of ${data.parent}` : n.label}
    >
      <Handle type="target" position={Position.Left} />
      {!data.compact && <div className="kind">{data.parent ? `↳ fan-out of ${data.parent}` : n.kind}</div>}
      <div className="tlabel">
        {data.compact && <StateMark state={n.health} />}
        {data.parent && data.compact && <span className="muted">↳ </span>}
        {n.label}
      </div>
      {!data.compact && (
        <div className="small muted">
          {n.health}
          {n.instances ? `, ${plural(n.instances, "instance")}` : ""}
        </div>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

function Group({ data }: NodeProps<GroupNode>) {
  return (
    <div className={`tgroup ${data.health}${data.selected ? " selected" : ""}${data.section ? " provider-section" : ""}`}>
      <Handle type="target" position={Position.Left} />
      <div className="tgroup-head">
        <span className="tgroup-name">{data.section && <span className="kind">provider </span>}{data.account}</span>
        <Badge state={data.health} />
        <span className="muted">
          {data.targets > 0 && `${plural(data.targets, "target")}, `}
          {plural(data.jobs, data.section ? "account" : "job")}
        </span>
      </div>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes = { tile: Tile, group: Group };

const worst = (hs: Health[]): Health =>
  hs.includes("failing") ? "failing" : hs.includes("degraded") ? "degraded" : hs.length && hs.every((h) => h === "ok") ? "ok" : "unknown";

export interface Layout {
  nodes: Node[];
  edges: Edge[];
  accounts: Map<string, string>; // job id -> account node id
}

interface Options {
  compact: boolean;
  keep?: (n: GraphNode) => boolean;
  match?: (n: GraphNode) => boolean;
  selected?: string | null;
}

const gap = 12;
// Minimum widths, so that the heads keep their names next to their state.
const minGroupW = 260;
const minSectionW = 400;
const head = 34;
const sectionHead = 60; // height of .tgroup.provider-section > .tgroup-head, plus a gap

// wires indexes the edges of the graph both ways.
function wires(graph: GraphT) {
  const byId = new Map(graph.nodes.map((n) => [n.id, n]));
  const incoming = new Map<string, string[]>();
  const outgoing = new Map<string, string[]>();
  for (const e of graph.edges) {
    incoming.set(e.to, [...(incoming.get(e.to) ?? []), e.from]);
    outgoing.set(e.from, [...(outgoing.get(e.from) ?? []), e.to]);
  }
  return { byId, incoming, outgoing };
}

// jobAccounts maps each job to the account node it runs for, through its
// fan-out parents ("" when none).
export function jobAccounts(graph: GraphT, w = wires(graph)): Map<string, string> {
  const { byId, incoming } = w;
  const accountOf = (id: string, seen = new Set<string>()): string | undefined => {
    if (seen.has(id)) return undefined;
    seen.add(id);
    for (const p of incoming.get(id) ?? []) {
      const pn = byId.get(p);
      if (pn?.kind === "account") return p;
      if (pn?.kind === "job") {
        const a = accountOf(p, seen);
        if (a) return a;
      }
    }
    return undefined;
  };
  const accounts = new Map<string, string>();
  for (const n of graph.nodes) if (n.kind === "job") accounts.set(n.id, accountOf(n.id) ?? "");
  return accounts;
}

// layout places the graph. Detailed: columns for providers, accounts and
// targets, the job groups of the accounts, then the outputs; a provider
// sits in front of its accounts, an account in front of its group.
// Compact: one section per provider holding the groups of its accounts in
// a grid, then the outputs. Inside a group, a fan-out child follows its
// parent and names it instead of being wired to it, so that wires never
// cross the tiles.
export function layout(graph: GraphT, opts: Options): Layout {
  const index = wires(graph);
  const { byId, incoming, outgoing } = index;
  const parentJob = (id: string) => (incoming.get(id) ?? []).find((p) => byId.get(p)?.kind === "job");
  const accounts = jobAccounts(graph, index);
  const keep = opts.keep ?? (() => true);
  const tileW = opts.compact ? 156 : 196;
  const tileH = opts.compact ? 34 : 66;
  const providers = graph.nodes.filter((n) => n.kind === "provider");
  const outputs = graph.nodes.filter((n) => n.kind === "output");
  const nodes: Node[] = [];
  const edges: Edge[] = [];
  const groupOf = new Map<string, string>();

  // The jobs of an account, each parent followed by its children.
  const jobsOf = (a: string) => {
    const all = graph.nodes.filter((n) => n.kind === "job" && accounts.get(n.id) === a && keep(n));
    const ids = new Set(all.map((j) => j.id));
    const ordered: GraphNode[] = [];
    const visit = (j: GraphNode) => {
      if (ordered.includes(j)) return;
      ordered.push(j);
      for (const c of outgoing.get(j.id) ?? []) {
        const cn = byId.get(c);
        if (cn && ids.has(c)) visit(cn);
      }
    };
    for (const j of all) if (!parentJob(j.id) || !ids.has(parentJob(j.id)!)) visit(j);
    for (const j of all) visit(j);
    return ordered;
  };

  // group returns the group node of an account and its tiles, at (x, y),
  // relative to parent when given.
  const group = (a: GraphNode, x: number, y: number, parent?: string) => {
    const jobs = jobsOf(a.id);
    const targets = (outgoing.get(a.id) ?? []).filter((id) => byId.get(id)?.kind === "target");
    const cols = Math.max(1, Math.min(opts.compact ? 6 : 4, Math.ceil(jobs.length / (opts.compact ? 4 : 3))));
    const rows = Math.max(1, Math.ceil(jobs.length / cols));
    // Wide enough for the head (name, state, counts) of a one-column group.
    const w = Math.max(cols * tileW + (cols + 1) * gap, minGroupW);
    const h = head + rows * (tileH + gap) + gap;
    const gid = "group:" + a.id;
    const health = worst(jobs.map((j) => j.health));
    nodes.push({
      id: gid, type: "group", position: { x, y }, parentId: parent, extent: parent ? "parent" : undefined,
      selected: opts.selected === gid, ariaLabel: `jobs of ${a.label}, ${health}`,
      data: { account: a.label, health, jobs: jobs.length, targets: targets.length, selected: opts.selected === gid },
      style: { width: w, height: h },
    });
    jobs.forEach((j, i) => {
      groupOf.set(j.id, gid);
      const p = parentJob(j.id);
      nodes.push({
        id: j.id, type: "tile", parentId: gid, extent: "parent", selected: opts.selected === j.id, ariaLabel: `job ${j.label}, ${j.health}`,
        position: { x: gap + (i % cols) * (tileW + gap), y: head + Math.floor(i / cols) * (tileH + gap) },
        data: { node: j, selected: opts.selected === j.id, dim: opts.match ? !opts.match(j) : false, compact: opts.compact, parent: p ? byId.get(p)?.label : undefined },
        style: { width: tileW },
      });
    });
    return { gid, w, h, targets, jobs: jobs.length };
  };

  const accountsOf = (p: GraphNode) =>
    (outgoing.get(p.id) ?? []).map((id) => byId.get(id)).filter((a): a is GraphNode => a?.kind === "account" && keep(a) && (!opts.keep || jobsOf(a.id).length > 0));

  let y = 0;
  let right = 0;
  const feeds = new Set<string>(); // "from|output": groups (or sections) feeding an output
  const feedFrom = (holder: string, jobIds: string[]) => {
    for (const j of jobIds) for (const o of outgoing.get(j) ?? []) if (byId.get(o)?.kind === "output") feeds.add(`${holder}|${o}`);
  };

  if (opts.compact) {
    for (const p of providers) {
      if (!keep(p)) continue;
      const accts = accountsOf(p);
      if (accts.length === 0) continue;
      const sid = "section:" + p.id;
      const cols = Math.max(1, Math.min(4, Math.ceil(Math.sqrt(accts.length / 1.2))));
      nodes.push({ id: sid, type: "group", position: { x: 0, y }, selected: opts.selected === sid, ariaLabel: `provider ${p.label}, ${p.health}`, data: { account: p.label, health: p.health, jobs: accts.length, targets: 0, selected: opts.selected === sid, section: true }, style: {} });
      const colY = Array<number>(cols).fill(sectionHead);
      const colX = Array<number>(cols).fill(0);
      const colW = Array<number>(cols).fill(0);
      const placed: { gid: string; col: number }[] = [];
      accts.forEach((a, i) => {
        const col = i % cols;
        const g = group(a, 0, colY[col]!, sid);
        placed.push({ gid: g.gid, col });
        colY[col] = colY[col]! + g.h + 16;
        colW[col] = Math.max(colW[col]!, g.w);
        feedFrom(sid, jobsOf(a.id).map((j) => j.id));
      });
      for (let c = 1; c < cols; c++) colX[c] = colX[c - 1]! + colW[c - 1]! + 16;
      for (const { gid, col } of placed) nodes.find((n) => n.id === gid)!.position.x = 16 + colX[col]!;
      const w = Math.max(16 + colX[cols - 1]! + colW[cols - 1]! + 16, minSectionW);
      const h = Math.max(...colY) + 4;
      nodes.find((n) => n.id === sid)!.style = { width: w, height: h };
      right = Math.max(right, w);
      y += h + 32;
    }
  } else {
    const col = { provider: 0, account: 240, target: 480, jobs: 720 };
    const centers = new Map<string, number>();
    for (const p of providers) {
      if (!keep(p)) continue;
      for (const a of accountsOf(p)) {
        const g = group(a, col.jobs, y);
        const cy = y + g.h / 2;
        centers.set(a.id, cy);
        nodes.push(tile(a, col.account, cy - 33, opts));
        edges.push(edge(a.id, g.gid));
        g.targets.forEach((t, i) => {
          nodes.push(tile(byId.get(t)!, col.target, cy - 33 + (i - (g.targets.length - 1) / 2) * 80, opts));
          edges.push(edge(a.id, t));
        });
        feedFrom(g.gid, jobsOf(a.id).map((j) => j.id));
        right = Math.max(right, col.jobs + g.w);
        y += g.h + 28;
      }
      const ys = accountsOf(p).map((a) => centers.get(a.id)).filter((v): v is number => v !== undefined);
      if (ys.length === 0) continue;
      nodes.push(tile(p, col.provider, ys.reduce((a, b) => a + b, 0) / ys.length - 33, opts));
      for (const a of accountsOf(p)) edges.push(edge(p.id, a.id));
    }
  }
  const outX = right + 120;
  outputs.forEach((o, i) => nodes.push(tile(o, outX, Math.max(0, y / 2 - 33) + (i - (outputs.length - 1) / 2) * 90, opts)));
  for (const k of feeds) {
    const [from, o] = k.split("|") as [string, string];
    edges.push(edge(from, o, true));
  }
  return { nodes, edges, accounts };
}

function tile(n: GraphNode, x: number, y: number, opts: { selected?: string | null }): TileNode {
  return {
    id: n.id, type: "tile", position: { x, y }, selected: opts.selected === n.id, ariaLabel: `${n.kind} ${n.label}, ${n.health}`, data: { node: n, selected: opts.selected === n.id, dim: false, compact: false },
    style: { width: 196 },
  };
}

function edge(from: string, to: string, out = false): Edge {
  return { id: `${from}->${to}`, source: from, target: to, style: { stroke: out ? "var(--cyan-2)" : "var(--ink-2)", opacity: out ? 0.8 : 0.6 } };
}

// selectionAfter is the node selected once React Flow applied changes: the
// last node selected (a click, or Enter or Space on a focused node), none
// when the selected node is unselected (Escape, a click on the pane), or
// the current one when the changes do not touch the selection.
export function selectionAfter(changes: NodeChange[], current: string | null): string | null {
  let next = current;
  for (const c of changes) {
    if (c.type !== "select") continue;
    if (c.selected) next = c.id;
    else if (c.id === next) next = null;
  }
  return next;
}

export function Graph() {
  const colorMode = useColorMode();
  const graph = useJSON<GraphT>("/graph");
  const accountList = useList<Account>("/accounts");
  const [selected, setSelected] = useState<string | null>(null);
  const [provider, setProvider] = useState("");
  const [account, setAccount] = useState("");
  const [query, setQuery] = useState("");
  const [compactChoice, setCompact] = useState<boolean | null>(null);
  const data = graph.data;
  const jobsCount = data?.nodes.filter((n) => n.kind === "job").length ?? 0;
  const compact = compactChoice ?? jobsCount > 30;
  const index = useMemo(() => {
    if (!data) return undefined;
    const byId = new Map(data.nodes.map((n) => [n.id, n]));
    const providerOf = new Map<string, string>();
    for (const e of data.edges) if (byId.get(e.from)?.kind === "provider") providerOf.set(e.to, e.from);
    return { providerOf, accounts: jobAccounts(data) };
  }, [data]);
  const { nodes, edges } = useMemo(() => {
    if (!data || !index) return { nodes: [] as Node[], edges: [] as Edge[] };
    const { providerOf, accounts } = index;
    const filtering = provider !== "" || account !== "" || query !== "";
    const q = query.trim().toLowerCase();
    const keep = (n: GraphNode) => {
      if (n.kind === "provider") return !provider || n.id === provider;
      if (n.kind === "account") return (!account || n.id === account) && (!provider || providerOf.get(n.id) === provider);
      if (n.kind === "job") {
        const a = accounts.get(n.id) ?? "";
        return (!account || a === account) && (!provider || providerOf.get(a) === provider) && (!q || n.label.toLowerCase().includes(q));
      }
      return true;
    };
    return layout(data, { compact, keep: filtering ? keep : undefined, match: q ? (n) => n.label.toLowerCase().includes(q) : undefined, selected });
  }, [data, index, selected, provider, account, query, compact]);
  if (graph.error && !data) return <Failure error={graph.error} />;
  if (!data) return <Loading />;
  const sel = data.nodes.find((n) => n.id === selected);
  const selGroup = selected?.startsWith("group:") || selected?.startsWith("section:") ? data.nodes.find((n) => "group:" + n.id === selected || "section:" + n.id === selected) : undefined;
  const providers = data.nodes.filter((n) => n.kind === "provider");
  const accounts = data.nodes.filter((n) => n.kind === "account");
  const reasons = sel?.kind === "account" ? accountList.data?.find((a) => a.name === sel.label)?.health_reasons ?? [] : [];
  return (
    <>
      <PageHead title="Graph" sub="Providers, accounts and targets, the jobs of each account, and the outputs. Select a node (click it, or Tab to it and press Enter) for its details; Escape closes them.">
        <Select label="Provider" value={provider} onChange={(v) => { setProvider(v); setAccount(""); }}>
          <option value="">All providers</option>
          {providers.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </Select>
        <Select label="Account" value={account} onChange={setAccount}>
          <option value="">All accounts</option>
          {accounts.map((a) => <option key={a.id} value={a.id}>{a.label}</option>)}
        </Select>
        <input className="search" type="search" placeholder="Search jobs" aria-label="Search jobs" value={query} onChange={(e) => setQuery(e.target.value)} />
        <button type="button" className="btn" aria-pressed={compact} onClick={() => setCompact(!compact)}>{compact ? "Detailed tiles" : "Compact tiles"}</button>
      </PageHead>
      <div className="graph" style={{ position: "relative" }}>
        <ReactFlow
          key={`${provider}|${account}|${compact}`}
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView
          minZoom={0.1}
          nodesDraggable={false}
          nodesConnectable={false}
          edgesFocusable={false}
          proOptions={{ hideAttribution: true }}
          onNodeClick={(_, n) => setSelected(n.id)}
          onNodesChange={(changes) => setSelected((cur) => selectionAfter(changes, cur))}
          onPaneClick={() => setSelected(null)}
          colorMode={colorMode}
        >
          <Background color="var(--line)" gap={24} />
          <Controls showInteractive={false} />
        </ReactFlow>
        {(sel || selGroup) && (
          <div className="card panel" role="dialog" aria-label={sel ? `${sel.kind} ${sel.label}` : selected?.startsWith("section:") ? `provider section ${selGroup!.label}` : `jobs of ${selGroup!.label}`}>
            <div className="row" style={{ justifyContent: "space-between" }}>
              <h3 style={{ margin: 0 }}>{(sel ?? selGroup)!.label}</h3>
              <button className="btn" onClick={() => setSelected(null)} aria-label="Close">Close</button>
            </div>
            <dl className="kv small" style={{ marginTop: 10 }}>
              <dt>Kind</dt><dd>{sel ? sel.kind : selected?.startsWith("section:") ? "provider section" : "jobs of the account"}</dd>
              <dt>Health</dt><dd><Badge state={(sel ?? selGroup)!.health} /></dd>
              {sel?.instances ? (<><dt>Instances</dt><dd>{sel.instances}</dd></>) : null}
            </dl>
            {reasons.length > 0 && (
              <ul className="reasons small mono">
                {reasons.map((r) => <li key={r}>{r}</li>)}
              </ul>
            )}
            <p className="small" style={{ marginBottom: 0 }}>
              {sel?.kind === "job" && <a href={href(["jobs", sel.label])}>Job details</a>}
              {(sel?.kind === "account" || sel?.kind === "provider" || selGroup) && <a href={href(["budgets"])}>Budgets</a>}
              {sel?.kind === "output" && <a href={href(["outputs"])}>Outputs</a>}
            </p>
          </div>
        )}
      </div>
    </>
  );
}
