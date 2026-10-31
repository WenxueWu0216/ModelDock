import { useCallback, useEffect, useRef, useState } from "react";
import { friendlyMessage, gql } from "../api";
import { KindTag, StatusBadge } from "../components/StatusBadge";
import { fmtTime } from "../format";
import { useLive, useLiveEvents } from "../live";
import { Link } from "../router";

interface DeploymentLite {
  version: string;
  status: string;
  kind: string;
  revision: number;
}

interface ModelRow {
  name: string;
  description: string;
  staging: DeploymentLite | null;
  prod: DeploymentLite | null;
}

interface ModelDetail {
  name: string;
  description: string;
  createdBy: string;
  createdAt: string;
  versions: { version: string; state: string; image: string; createdBy: string; createdAt: string }[];
  staging: DeploymentLite | null;
  prod: DeploymentLite | null;
  deployments: (DeploymentLite & { environment: string; createdBy: string; createdAt: string; reason: string | null })[];
}

// One GraphQL round trip instead of GET /models plus GET /models/{m}/status per model.
const MODELS_QUERY = `{
  models {
    name description
    staging { version status kind revision }
    prod { version status kind revision }
  }
}`;

// One GraphQL round trip instead of four REST calls (model, versions, status, history)
// plus the pending approvals for it.
const MODEL_QUERY = `query Model($name: String!) {
  model(name: $name) {
    name description createdBy createdAt
    versions(limit: 20) { version state image createdBy createdAt }
    staging { version status kind revision }
    prod { version status kind revision }
    deployments(limit: 10) { version environment status kind revision createdBy createdAt reason }
  }
  approvals(status: "pending") { id modelName version requestedBy }
}`;

/** Refetches (debounced) whenever a live event for the given model arrives. */
function useRefetchOnEvents(load: () => void, model?: string) {
  const t = useRef<ReturnType<typeof setTimeout>>();
  const { epoch } = useLive();
  useEffect(load, [load, epoch]);
  useEffect(() => () => clearTimeout(t.current), []);
  useLiveEvents((m) => {
    if (model && m.model !== model) return;
    clearTimeout(t.current);
    t.current = setTimeout(load, 250);
  });
}

function EnvCell({ d }: { d: DeploymentLite | null }) {
  if (!d) return <span className="muted">—</span>;
  return (
    <span>
      {d.version} <StatusBadge status={d.status} />
    </span>
  );
}

export function Models() {
  const [rows, setRows] = useState<ModelRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const load = useCallback(() => {
    gql<{ models: ModelRow[] }>(MODELS_QUERY)
      .then((d) => setRows(d.models))
      .catch((e) => setErr(friendlyMessage(e)));
  }, []);
  useRefetchOnEvents(load);
  return (
    <section>
      <h2>Models</h2>
      {err && <p className="error">{err}</p>}
      {rows?.length === 0 && <p className="empty">No models registered yet.</p>}
      {rows && rows.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Model</th>
              <th>Staging</th>
              <th>Prod</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((m) => (
              <tr key={m.name}>
                <td>
                  <Link to={`/models/${m.name}`}>{m.name}</Link>
                  <div className="muted small">{m.description}</div>
                </td>
                <td>
                  <EnvCell d={m.staging} />
                </td>
                <td>
                  <EnvCell d={m.prod} />
                </td>
                <td className="small">
                  <Link to={`/timeline?model=${m.name}`}>timeline</Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

export function ModelPage({ name }: { name: string }) {
  const [data, setData] = useState<{ model: ModelDetail | null; approvals: { id: string; modelName: string; version: string; requestedBy: string }[] } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const load = useCallback(() => {
    gql<typeof data>(MODEL_QUERY, { name })
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => setErr(friendlyMessage(e)));
  }, [name]);
  useRefetchOnEvents(load, name);

  if (err) return <p className="error">{err}</p>;
  if (!data) return <p className="muted">loading…</p>;
  const m = data.model;
  if (!m) return <p className="error">Model {name} not found.</p>;
  const pending = data.approvals.filter((a) => a.modelName === name);
  return (
    <section>
      <h2>{m.name}</h2>
      <p className="muted">
        {m.description || "No description."} Registered by {m.createdBy} on {fmtTime(m.createdAt)}.
      </p>
      <div className="cards-row">
        <div className="card">
          <h4>staging</h4>
          <EnvCell d={m.staging} />
        </div>
        <div className="card">
          <h4>prod</h4>
          <EnvCell d={m.prod} />
        </div>
        <div className="card">
          <h4>pending approvals</h4>
          {pending.length === 0 ? (
            <span className="muted">none</span>
          ) : (
            <Link to="/approvals">
              {pending.map((a) => a.version).join(", ")}
            </Link>
          )}
        </div>
      </div>
      <h3>Versions</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Version</th>
            <th>State</th>
            <th>Image</th>
            <th>Registered</th>
          </tr>
        </thead>
        <tbody>
          {m.versions.map((v) => (
            <tr key={v.version}>
              <td>{v.version}</td>
              <td>
                <StatusBadge status={v.state} />
              </td>
              <td className="mono small">{v.image}</td>
              <td className="small">
                {v.createdBy} · {fmtTime(v.createdAt)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <h3>
        Recent revisions <Link to={`/timeline?model=${m.name}`} className="small">full timeline →</Link>
      </h3>
      <table className="grid">
        <tbody>
          {m.deployments.map((d) => (
            <tr key={`${d.environment}-${d.revision}`}>
              <td>{d.environment}</td>
              <td>r{d.revision}</td>
              <td>{d.version}</td>
              <td>
                <KindTag kind={d.kind} />
              </td>
              <td>
                <StatusBadge status={d.status} title={d.reason ?? undefined} />
              </td>
              <td className="small">
                {d.createdBy} · {fmtTime(d.createdAt)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {m.versions.length >= 2 && (
        <p>
          <Link to={`/diff?model=${m.name}&from=${m.versions[1]!.version}&to=${m.versions[0]!.version}`}>
            Diff {m.versions[1]!.version} → {m.versions[0]!.version}
          </Link>
        </p>
      )}
    </section>
  );
}
