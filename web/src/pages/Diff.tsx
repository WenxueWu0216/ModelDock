import { useEffect, useState } from "react";
import { friendlyMessage, rest } from "../api";
import { DiffTable } from "../components/DiffTable";
import { ModelPicker } from "../components/ModelPicker";
import { navigate, type Route } from "../router";
import type { ModelVersion, VersionDiff } from "../types";

export function Diff({ route }: { route: Route }) {
  const model = route.query.get("model") ?? "";
  const from = route.query.get("from") ?? "";
  const to = route.query.get("to") ?? "";
  const [versions, setVersions] = useState<ModelVersion[]>([]);
  const [diff, setDiff] = useState<VersionDiff | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const go = (q: { model?: string; from?: string; to?: string }) => navigate("/diff", { model, from, to, ...q });

  useEffect(() => {
    if (!model) return;
    rest<ModelVersion[]>(`/api/v1/models/${encodeURIComponent(model)}/versions?limit=100`)
      .then((vs) => {
        setVersions(vs);
        // Default: previous version -> newest version.
        if ((!from || !to) && vs.length >= 2) go({ from: vs[1]!.version, to: vs[0]!.version });
      })
      .catch((e) => setErr(friendlyMessage(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [model]);

  useEffect(() => {
    setDiff(null);
    if (!model || !from || !to) return;
    const q = new URLSearchParams({ from, to });
    rest<VersionDiff>(`/api/v1/models/${encodeURIComponent(model)}/diff?${q}`)
      .then((d) => {
        setDiff(d);
        setErr(null);
      })
      .catch((e) => setErr(friendlyMessage(e)));
  }, [model, from, to]);

  const select = (label: string, value: string, key: "from" | "to") => (
    <label className="inline">
      {label}{" "}
      <select value={value} aria-label={key} onChange={(e) => go({ [key]: e.target.value })}>
        <option value="">—</option>
        {versions.map((v) => (
          <option key={v.id} value={v.version}>
            {v.version} ({v.state.replace("_", " ")})
          </option>
        ))}
      </select>
    </label>
  );

  return (
    <section>
      <h2>Version diff</h2>
      <div className="toolbar">
        <ModelPicker value={model} onChange={(m) => navigate("/diff", { model: m })} />
        {select("From", from, "from")}
        {select("To", to, "to")}
      </div>
      {err && <p className="error">{err}</p>}
      {model && versions.length < 2 && !err && <p className="muted">This model needs at least two versions to compare.</p>}
      {diff && (
        <div className="card">
          <DiffTable diff={diff} />
        </div>
      )}
    </section>
  );
}
