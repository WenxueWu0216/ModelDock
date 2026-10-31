import { describe, expect, it } from "vitest";
import { applyEvent, currentByEnv } from "../timeline";
import type { Deployment, StreamMessage } from "../types";

const base = (over: Partial<Deployment>): Deployment => ({
  id: "x", modelName: "iris", version: "1.0.0", environment: "prod", revision: 1, kind: "promote",
  status: "ready", createdBy: "dana", createdAt: "2026-01-01T00:00:00Z", ...over,
});
const msg = (over: Partial<StreamMessage>): StreamMessage => ({
  seq: 1, eventId: "e", type: "deploy.ready", actor: "system:reconciler", occurredAt: "2026-01-01T00:01:00Z",
  model: "iris", environment: "prod", ...over,
});

describe("applyEvent", () => {
  it("marks the displaced revision rolled_back on a rollback", () => {
    const rows = [base({ id: "b", revision: 2, version: "1.1.0" }), base({ id: "a", status: "superseded" })];
    const out = applyEvent(rows, msg({ type: "rollback.started", deploymentId: "c", revision: 3, kind: "rollback", status: "pending", version: "1.0.0", actor: "dana" }), "iris");
    expect(out.map((d) => [d.id, d.status])).toEqual([["c", "pending"], ["b", "rolled_back"], ["a", "superseded"]]);
    expect(out[0]!.kind).toBe("rollback");
    expect(currentByEnv(out).prod?.id).toBe("c");
  });

  it("does not touch the other environment", () => {
    const rows = [base({ id: "s", environment: "staging" })];
    const out = applyEvent(rows, msg({ type: "promote.started", deploymentId: "p", status: "pending", kind: "promote" }), "iris");
    expect(out.find((d) => d.id === "s")!.status).toBe("ready");
  });

  it("records the failure reason", () => {
    const rows = [base({ id: "a", status: "progressing" })];
    const out = applyEvent(rows, msg({ type: "deploy.failed", deploymentId: "a", status: "failed", data: { reason: "not ready within 45s" } }), "iris");
    expect(out[0]).toMatchObject({ status: "failed", reason: "not ready within 45s" });
  });

  it("ignores events for other models, without a deployment, or for unknown non-pending revisions", () => {
    const rows = [base({ id: "a" })];
    expect(applyEvent(rows, msg({ model: "wine", deploymentId: "a", status: "failed" }), "iris")).toBe(rows);
    expect(applyEvent(rows, msg({ type: "approval.requested" }), "iris")).toBe(rows);
    expect(applyEvent(rows, msg({ deploymentId: "zzz", status: "ready" }), "iris")).toBe(rows);
  });
});
