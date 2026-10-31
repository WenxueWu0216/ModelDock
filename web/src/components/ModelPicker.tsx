import { useEffect, useState } from "react";
import { rest, friendlyMessage } from "../api";
import type { Model } from "../types";

/** Loads the model list and renders a <select>. Picks the first model if none is chosen. */
export function ModelPicker({ value, onChange }: { value: string; onChange: (name: string) => void }) {
  const [models, setModels] = useState<Model[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    rest<Model[]>("/api/v1/models")
      .then((ms) => {
        setModels(ms);
        if (!value && ms[0]) onChange(ms[0].name);
      })
      .catch((e) => setErr(friendlyMessage(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  if (err) return <span className="error">{err}</span>;
  if (!models) return <span className="muted">loading models…</span>;
  if (models.length === 0) return <span className="muted">No models registered yet.</span>;
  return (
    <label className="inline">
      Model{" "}
      <select value={value} onChange={(e) => onChange(e.target.value)} aria-label="model">
        {models.map((m) => (
          <option key={m.id} value={m.name}>
            {m.name}
          </option>
        ))}
      </select>
    </label>
  );
}
