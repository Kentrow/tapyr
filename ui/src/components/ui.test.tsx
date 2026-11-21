import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NextRun, Since, Table, type Column } from "./ui";

interface Row {
  name: string;
  calls: number;
}

const rows: Row[] = [
  { name: "devices", calls: 20 },
  { name: "alerts", calls: 300 },
  { name: "clients", calls: 5 },
];

const columns: Column<Row>[] = [
  { key: "name", label: "Name", sort: (r) => r.name, render: (r) => r.name },
  { key: "calls", label: "Calls", num: true, sort: (r) => r.calls, render: (r) => r.calls },
  { key: "note", label: "Note", render: () => "-" },
];

const names = () => screen.getAllByRole("row").slice(1).map((r) => r.firstElementChild?.textContent);
const header = (label: string) => screen.getByRole("columnheader", { name: new RegExp(`^${label}`) });

describe("Table", () => {
  afterEach(cleanup);

  it("keeps the order of the rows without a sort", () => {
    render(<Table rows={rows} columns={columns} rowKey={(r) => r.name} />);
    expect(names()).toEqual(["devices", "alerts", "clients"]);
    expect(header("Name").getAttribute("aria-sort")).toBeNull();
  });

  it("applies the initial sort", () => {
    render(<Table rows={rows} columns={columns} rowKey={(r) => r.name} initialSort={{ key: "calls", desc: true }} />);
    expect(names()).toEqual(["alerts", "devices", "clients"]);
    expect(header("Calls").getAttribute("aria-sort")).toBe("descending");
  });

  it("sorts by a column on click, then reverses on a second click", () => {
    render(<Table rows={rows} columns={columns} rowKey={(r) => r.name} />);
    fireEvent.click(screen.getByRole("button", { name: /^Calls/ }));
    expect(names()).toEqual(["clients", "devices", "alerts"]);
    expect(header("Calls").getAttribute("aria-sort")).toBe("ascending");
    fireEvent.click(screen.getByRole("button", { name: /^Calls/ }));
    expect(names()).toEqual(["alerts", "devices", "clients"]);
    expect(header("Calls").getAttribute("aria-sort")).toBe("descending");
    fireEvent.click(screen.getByRole("button", { name: /^Name/ }));
    expect(names()).toEqual(["alerts", "clients", "devices"]);
    expect(header("Calls").getAttribute("aria-sort")).toBeNull();
  });

  it("has no sort button on a column without sort", () => {
    render(<Table rows={rows} columns={columns} rowKey={(r) => r.name} />);
    expect(screen.queryByRole("button", { name: /^Note/ })).toBeNull();
  });

  it("says when there is nothing to show", () => {
    render(<Table rows={[]} columns={columns} rowKey={(r) => r.name} empty="No job." />);
    expect(screen.getByText("No job.")).toBeTruthy();
  });
});

// The relative times tick on their own: between two runs nothing else
// renders the page again, and "in 45s" stayed frozen until the next run.
describe("relative times", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("move with the clock, without a new render of the page", () => {
    vi.useFakeTimers();
    vi.setSystemTime(Date.parse("2026-11-03T12:00:00Z"));
    render(<p><Since at="2026-11-03T11:59:50Z" />, <NextRun at="2026-11-03T12:00:05Z" /></p>);
    expect(screen.getByRole("paragraph").textContent).toBe("10s ago, in 5s");
    act(() => vi.advanceTimersByTime(4000));
    expect(screen.getByRole("paragraph").textContent).toBe("14s ago, now");
    act(() => vi.advanceTimersByTime(5000));
    expect(screen.getByRole("paragraph").textContent).toBe("19s ago, due");
  });

  it("show the absolute time in their title", () => {
    render(<Since at="2026-11-03T11:59:50Z" />);
    expect(screen.getByText(/ago/).getAttribute("title")).toMatch(/2026/);
  });
});
