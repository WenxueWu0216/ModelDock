import { useCallback, useEffect, useState } from "react";
import { friendlyMessage, rest } from "../api";
import { can, useSession } from "../auth";
import { DiffTable } from "../components/DiffTable";
import { fmtAgo, fmtTime } from "../format";
import { useLive, useLiveEvents } from "../live";
import type { Approval, ModelStatus, VersionDiff } from "../types";

type DiffState =
  | { kind: "loading" }
  | { kind: "first"; }
  | { kind: "same"; version: string }
  | { kind: "diff"; diff: VersionDiff }
  | { kind: "error"; message: string };

/** Diff of the candidate against what runs in prod today. */
function useProdDiff(a: Approval): DiffState {
  const [state, setState] = useState<DiffState>({ kind: "loading" });
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const st = await rest<ModelStatus>(`/api/v1/models/${encodeURIComponent(a.modelName)}/status`);
        const prod = st.current.prod;
        if (!prod) return !cancelled && setState({ kind: "first" });
        if (prod.version === a.version) return !cancelled && setState({ kind: "same", version: prod.version });
        const q = new URLSearchParams({ from: prod.version, to: a.version });
        const diff = await rest<VersionDiff>(`/api/v1/models/${encodeURIComponent(a.modelName)}/diff?${q}`);
        if (!cancelled) setState({ kind: "diff", diff });
      } catch (e) {
        if (!cancelled) setState({ kind: "error", message: friendlyMessage(e) });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [a.modelName, a.version]);
  return state;
}

function ApprovalCard({ a, onDecided }: { a: Approval; onDecided: (id: string) => void }) {
  const session = useSession();
  const diff = useProdDiff(a);
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const mayDecide = can.approve(session.role);

  const decide = async (verb: "approve" | "reject") => {
    setBusy(true);
    setErr(null);
    try {
      await rest<Approval>(`/api/v1/approvals/${a.id}/${verb}`, { method: "POST", body: { comment } });
      onDecided(a.id);
    } catch (e) {
      setErr(friendlyMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <article className="card approval" aria-label={`promotion ${a.modelName} ${a.version}`}>
      <header>
        <h3>
          {a.modelName} <span className="version">{a.version}</span> → prod
        </h3>
        <span className="muted small" title={fmtTime(a.requestedAt)}>
          requested by <strong>{a.requestedBy}</strong> {fmtAgo(a.requestedAt)}
        </span>
      </header>
      {a.comment && <blockquote>{a.comment}</blockquote>}
      <section className="inline-diff">
        <h4>Change against prod</h4>
        {diff.kind === "loading" && <p className="muted">loading diff…</p>}
        {diff.kind === "first" && <p className="muted">Nothing in prod yet: this is the first promotion.</p>}
        {diff.kind === "same" && <p className="muted">{diff.version} is already the prod version.</p>}
        {diff.kind === "diff" && <DiffTable diff={diff.diff} compact />}
        {diff.kind === "error" && <p className="error">{diff.message}</p>}
      </section>
      {mayDecide ? (
        <footer className="actions">
          <input
            aria-label="comment"
            placeholder="comment (optional)"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
          />
          <button onClick={() => decide("approve")} disabled={busy} className="primary">
            Approve
          </button>
          <button onClick={() => decide("reject")} disabled={busy} className="danger">
            Reject
          </button>
        </footer>
      ) : (
        <p className="muted small">Your role ({session.role}) cannot approve or reject promotions.</p>
      )}
      {err && (
        <p className="error" role="alert">
          {err}
        </p>
      )}
    </article>
  );
}

export function Approvals() {
  const [items, setItems] = useState<Approval[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const { epoch } = useLive();

  const load = useCallback(() => {
    rest<Approval[]>("/api/v1/approvals?status=pending")
      .then((as) => {
        setItems(as);
        setErr(null);
      })
      .catch((e) => setErr(friendlyMessage(e)));
  }, []);
  useEffect(load, [load, epoch]);
  useLiveEvents((m) => {
    if (m.type.startsWith("approval.")) load();
  });

  return (
    <section>
      <h2>Approval queue</h2>
      <p className="muted">
        Pending promotion requests to prod. The approver must be someone other than the requester; the API
        enforces this.
      </p>
      {err && <p className="error">{err}</p>}
      {items === null && !err && <p className="muted">loading…</p>}
      {items?.length === 0 && <p className="empty">No pending promotion requests.</p>}
      <div className="stack">
        {items?.map((a) => (
          <ApprovalCard key={a.id} a={a} onDecided={(id) => setItems((xs) => xs?.filter((x) => x.id !== id) ?? null)} />
        ))}
      </div>
    </section>
  );
}
