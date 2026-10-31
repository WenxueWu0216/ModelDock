import { render, screen, within } from "@testing-library/react";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LiveIndicator } from "../components/LiveIndicator";
import { Timeline } from "../pages/Timeline";
import { parseHash } from "../router";
import type { Deployment } from "../types";
import { FakeEventSource, mockFetch, Providers, session } from "./fakes";

const t0 = "2026-10-31T10:00:00Z";
const history: Deployment[] = [
  { id: "d2", modelName: "iris", version: "1.0.0", environment: "prod", revision: 1, kind: "promote", status: "ready", createdBy: "dana", createdAt: t0 },
  { id: "d1", modelName: "iris", version: "1.0.0", environment: "staging", revision: 1, kind: "deploy", status: "ready", createdBy: "dana", createdAt: t0 },
];

/** Lets pending fetches resolve inside act(). */
const settle = () => act(async () => {
  await new Promise((r) => setTimeout(r, 0));
});

function setup() {
  const { fn } = mockFetch({
    "GET /api/v1/models": { body: [{ id: "m1", name: "iris", description: "", createdBy: "dana", createdAt: t0 }] },
    "GET /api/v1/models/iris/deployments": { body: history },
  });
  render(
    <Providers s={session("vic", "viewer")}>
      <LiveIndicator />
      <Timeline route={parseHash("#/timeline?model=iris")} />
    </Providers>,
  );
  const deploymentFetches = () => fn.mock.calls.filter(([u]) => String(u).includes("/deployments")).length;
  return { deploymentFetches };
}

describe("Deployment timeline", () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
  });
  afterEach(() => vi.unstubAllGlobals());

  it("renders revisions per environment with kind, actor and status", async () => {
    setup();
    const prod = await screen.findByRole("region", { name: "prod timeline" });
    const rev = within(prod).getByTestId("rev-d2");
    expect(rev).toHaveTextContent("r1");
    expect(rev).toHaveTextContent("1.0.0");
    expect(rev).toHaveTextContent("promote");
    expect(rev).toHaveTextContent("dana");
    expect(within(rev).getByTestId("status-badge")).toHaveTextContent("ready");
    expect(FakeEventSource.last().url).toContain("access_token=tok-vic-viewer");
    await settle();
  });

  it("updates the timeline and badges in place from live events, without refetching", async () => {
    const { deploymentFetches } = setup();
    const prod = await screen.findByRole("region", { name: "prod timeline" });
    const es = FakeEventSource.last();
    act(() => es.open());
    expect(screen.getByTestId("live-indicator")).toHaveTextContent("live");
    // open() bumps the connection epoch, which reloads once; count after that.
    await vi.waitFor(() => expect(deploymentFetches()).toBe(2));
    await settle();
    const before = deploymentFetches();

    // A promotion starts: new revision r2 appears pending, r1 is superseded.
    act(() =>
      es.emit({ seq: 10, type: "promote.started", model: "iris", version: "1.1.0", environment: "prod", deploymentId: "d3", revision: 2, kind: "promote", status: "pending", actor: "dana" }),
    );
    const r2 = within(prod).getByTestId("rev-d3");
    expect(within(r2).getByTestId("status-badge")).toHaveTextContent("pending");
    expect(within(within(prod).getByTestId("rev-d2")).getByTestId("status-badge")).toHaveTextContent("superseded");
    expect(within(prod).getByTestId("current-prod")).toHaveTextContent("1.1.0");

    act(() =>
      es.emit({ seq: 11, type: "deploy.progressing", model: "iris", version: "1.1.0", environment: "prod", deploymentId: "d3", revision: 2, kind: "promote", status: "progressing" }),
    );
    expect(within(r2).getByTestId("status-badge")).toHaveTextContent("progressing");

    act(() =>
      es.emit({ seq: 12, type: "deploy.ready", model: "iris", version: "1.1.0", environment: "prod", deploymentId: "d3", revision: 2, kind: "promote", status: "ready" }),
    );
    expect(within(r2).getByTestId("status-badge")).toHaveTextContent("ready");
    expect(within(within(prod).getByTestId("current-prod")).getByTestId("status-badge")).toHaveTextContent("ready");

    // A duplicate (replayed) event is ignored, and other models' events too.
    act(() =>
      es.emit({ seq: 11, type: "deploy.progressing", model: "iris", environment: "prod", deploymentId: "d3", status: "progressing" }),
    );
    act(() =>
      es.emit({ seq: 13, type: "deploy.failed", model: "wine", environment: "prod", deploymentId: "d3", status: "failed" }),
    );
    expect(within(r2).getByTestId("status-badge")).toHaveTextContent("ready");
    expect(deploymentFetches()).toBe(before);
  });

  it("shows reconnecting, resumes from the last event id and reloads on reconnect", async () => {
    const { deploymentFetches } = setup();
    await screen.findByRole("region", { name: "prod timeline" });
    const es = FakeEventSource.last();
    act(() => es.open());
    await vi.waitFor(() => expect(deploymentFetches()).toBe(2));
    await settle();
    act(() => es.emit({ seq: 42, type: "registry.model_registered", model: "iris" }));
    const before = deploymentFetches();

    // Browser gave up (e.g. server restarted): we reopen with lastEventId.
    act(() => es.fail(true));
    expect(screen.getByTestId("live-indicator")).toHaveTextContent("reconnecting");
    await vi.waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    const es2 = FakeEventSource.last();
    expect(es2.url).toContain("lastEventId=42");
    act(() => es2.open());
    expect(screen.getByTestId("live-indicator")).toHaveTextContent("live");
    await vi.waitFor(() => expect(deploymentFetches()).toBe(before + 1));
    await settle();
  });
});
