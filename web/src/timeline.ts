import type { Deployment, DeploymentStatus, StreamMessage } from "./types";

const LIVE_OR_IN_FLIGHT: DeploymentStatus[] = ["pending", "progressing", "ready"];

/**
 * Folds one stream event into a model's deployment history (newest first)
 * and returns a new array, or the same array if the event does not apply.
 *
 * - A pending event for an unknown deployment is a new revision: it is
 *   prepended, and the previous live revision in that environment becomes
 *   superseded (deploy/promote) or rolled_back (rollback), mirroring the
 *   server's displaceCurrent.
 * - Any other status event updates the matching revision in place.
 */
export function applyEvent(rows: Deployment[], m: StreamMessage, model: string): Deployment[] {
  if (m.model !== model || !m.deploymentId || !m.status || !m.environment) return rows;
  const idx = rows.findIndex((d) => d.id === m.deploymentId);
  if (idx >= 0) {
    const cur = rows[idx]!;
    const next: Deployment = { ...cur, status: m.status, updatedAt: m.occurredAt };
    if (m.status === "progressing") next.startedAt = m.occurredAt;
    if (m.status === "ready") {
      next.readyAt = m.occurredAt;
      next.finishedAt = m.occurredAt;
    }
    if (m.status === "failed") {
      next.finishedAt = m.occurredAt;
      const reason = m.data?.reason;
      if (typeof reason === "string") next.reason = reason;
    }
    const out = rows.slice();
    out[idx] = next;
    return out;
  }
  if (m.status !== "pending") return rows; // e.g. ready for a revision older than the loaded page
  const kind = m.kind ?? "deploy";
  const displacedAs: DeploymentStatus = kind === "rollback" || kind === "auto_rollback" ? "rolled_back" : "superseded";
  const added: Deployment = {
    id: m.deploymentId,
    modelName: model,
    version: m.version ?? "",
    environment: m.environment,
    revision: m.revision ?? 0,
    kind,
    status: "pending",
    reason: typeof m.data?.reason === "string" && m.data.reason ? m.data.reason : undefined,
    createdBy: m.actor,
    createdAt: m.occurredAt,
  };
  const out = rows.map((d) =>
    d.environment === m.environment && LIVE_OR_IN_FLIGHT.includes(d.status)
      ? { ...d, status: displacedAs, finishedAt: m.occurredAt }
      : d,
  );
  return [added, ...out];
}

/** The current (highest revision) deployment per environment. */
export function currentByEnv(rows: Deployment[]): Partial<Record<string, Deployment>> {
  const out: Partial<Record<string, Deployment>> = {};
  for (const d of rows) {
    const c = out[d.environment];
    if (!c || d.revision > c.revision) out[d.environment] = d;
  }
  return out;
}
