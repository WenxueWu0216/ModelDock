import { useCallback, useEffect, useRef, useState } from "react";
import { friendlyMessage, rest } from "../api";
import { ModelPicker } from "../components/ModelPicker";
import { KindTag, StatusBadge } from "../components/StatusBadge";
import { fmtTime } from "../format";
import { useLive, useLiveEvents } from "../live";
import { navigate, type Route } from "../router";
import { applyEvent, currentByEnv } from "../timeline";
import type { Deployment, Environment } from "../types";

const ENVS: Environment[] = ["staging", "prod"];

function Lane({ env, rows, flash }: { env: Environment; rows: Deployment[]; flash: Set<string> }) {
  const current = currentByEnv(rows)[env];
  return (
    <section className="lane" aria-label={`${env} timeline`}>
      <header>
        <h3>{env}</h3>
        {current ? (
          <span className="current" data-testid={`current-${env}`}>
            {current.version} <StatusBadge status={current.status} />
          </span>
        ) : (
          <span className="muted">never deployed</span>
        )}
      </header>
      <ol className="timeline">
        {rows.length === 0 && <li className="muted">No revisions.</li>}
        {rows.map((d) => (
          <li key={d.id} className={`rev rev-${d.status} ${flash.has(d.id) ? "flash" : ""}`} data-testid={`rev-${d.id}`}>
            <div className="rev-head">
              <span className="revno">r{d.revision}</span>
              <span className="version">{d.version}</span>
              <KindTag kind={d.kind} />
              <StatusBadge status={d.status} title={d.reason} />
            </div>
            <div className="rev-meta muted small">
              by <strong>{d.createdBy}</strong> · <time dateTime={d.createdAt}>{fmtTime(d.createdAt)}</time>
              {d.readyAt && <> · ready {fmtTime(d.readyAt)}</>}
              {d.status === "failed" && d.finishedAt && <> · failed {fmtTime(d.finishedAt)}</>}
            </div>
            {d.reason && <div className="reason small">{d.reason}</div>}
          </li>
        ))}
      </ol>
    </section>
  );
}

export function Timeline({ route }: { route: Route }) {
  const model = route.query.get("model") ?? "";
  const [rows, setRows] = useState<Deployment[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [flash, setFlash] = useState<Set<string>>(new Set());
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const { epoch } = useLive();

  const load = useCallback(() => {
    if (!model) return;
    rest<Deployment[]>(`/api/v1/models/${encodeURIComponent(model)}/deployments?limit=100`)
      .then((ds) => {
        setRows(ds);
        setErr(null);
      })
      .catch((e) => setErr(friendlyMessage(e)));
  }, [model]);
  // Reload on model change and on every (re)connect of the stream, so events
  // missed while disconnected never leave the view stale.
  useEffect(load, [load, epoch]);
  useEffect(() => {
    const ts = timers.current;
    return () => ts.forEach(clearTimeout);
  }, []);

  useLiveEvents((m) => {
    if (m.model !== model || !m.deploymentId) return;
    setRows((cur) => (cur ? applyEvent(cur, m, model) : cur));
    const id = m.deploymentId;
    setFlash((s) => new Set(s).add(id));
    clearTimeout(timers.current.get(id));
    timers.current.set(
      id,
      setTimeout(() => {
        setFlash((s) => {
          const n = new Set(s);
          n.delete(id);
          return n;
        });
      }, 1500),
    );
  });

  return (
    <section>
      <h2>Deployment timeline</h2>
      <div className="toolbar">
        <ModelPicker value={model} onChange={(m) => navigate("/timeline", { model: m })} />
      </div>
      {err && <p className="error">{err}</p>}
      {rows && (
        <div className="lanes">
          {ENVS.map((env) => (
            <Lane key={env} env={env} rows={rows.filter((d) => d.environment === env)} flash={flash} />
          ))}
        </div>
      )}
    </section>
  );
}
