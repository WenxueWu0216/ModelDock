export function StatusBadge({ status, title }: { status: string; title?: string }) {
  return (
    <span className={`badge badge-${status}`} title={title} data-testid="status-badge">
      {status.replace("_", " ")}
    </span>
  );
}

export function KindTag({ kind }: { kind: string }) {
  return <span className={`kind kind-${kind}`}>{kind.replace("_", " ")}</span>;
}
