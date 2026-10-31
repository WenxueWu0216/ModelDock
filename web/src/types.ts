// Shapes returned by the ModelDock REST API (see domain/types.go) and the
// SSE event contract (see README "Live event stream").

export type Role = "viewer" | "deployer" | "approver" | "admin";
export type Environment = "staging" | "prod";
export type DeploymentKind = "deploy" | "promote" | "rollback" | "auto_rollback";
export type DeploymentStatus =
  | "pending"
  | "progressing"
  | "ready"
  | "failed"
  | "superseded"
  | "rolled_back";

export interface Model {
  id: string;
  name: string;
  description: string;
  createdBy: string;
  createdAt: string;
}

export interface ModelVersion {
  id: string;
  modelName: string;
  version: string;
  image: string;
  state: string;
  createdBy: string;
  createdAt: string;
}

export interface Deployment {
  id: string;
  modelName: string;
  version: string;
  environment: Environment;
  revision: number;
  kind: DeploymentKind;
  status: DeploymentStatus;
  reason?: string;
  createdBy: string;
  createdAt: string;
  updatedAt?: string;
  startedAt?: string;
  readyAt?: string;
  finishedAt?: string;
}

export interface Approval {
  id: string;
  versionId: string;
  modelName: string;
  version: string;
  status: "pending" | "approved" | "rejected";
  requestedBy: string;
  requestedAt: string;
  decidedBy?: string;
  decidedAt?: string;
  comment?: string;
}

export interface FieldChange {
  field: string;
  from: string;
  to: string;
}

export interface VersionDiff {
  model: string;
  from: string;
  to: string;
  changes: FieldChange[];
}

export interface ModelStatus {
  model: Model;
  current: Partial<Record<Environment, Deployment | null>>;
}

/** One SSE event's data (stream.Message on the server). */
export interface StreamMessage {
  seq: number;
  eventId: string;
  type: string;
  actor: string;
  model?: string;
  version?: string;
  environment?: Environment;
  deploymentId?: string;
  revision?: number;
  kind?: DeploymentKind;
  status?: DeploymentStatus;
  approvalId?: string;
  occurredAt: string;
  data?: Record<string, unknown>;
}

/** Event names the server sends; EventSource needs one listener per name. */
export const STREAM_EVENT_TYPES = [
  "registry.model_registered",
  "registry.version_registered",
  "deploy.requested",
  "deploy.progressing",
  "deploy.ready",
  "deploy.failed",
  "promote.started",
  "rollback.started",
  "approval.requested",
  "approval.approved",
  "approval.rejected",
  "inference_health.changed",
] as const;
