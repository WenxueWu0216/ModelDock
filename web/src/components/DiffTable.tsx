import type { VersionDiff } from "../types";

export function DiffTable({ diff, compact }: { diff: VersionDiff; compact?: boolean }) {
  if (diff.changes.length === 0) {
    return <p className="muted">No configuration differences between {diff.from} and {diff.to}.</p>;
  }
  return (
    <table className={`diff ${compact ? "diff-compact" : ""}`} aria-label={`diff ${diff.from} to ${diff.to}`}>
      <thead>
        <tr>
          <th>Field</th>
          <th>{diff.from}</th>
          <th>{diff.to}</th>
        </tr>
      </thead>
      <tbody>
        {diff.changes.map((c) => (
          <tr key={c.field}>
            <td className="field">{c.field}</td>
            <td className="from">{c.from === "" ? <span className="muted">(unset)</span> : c.from}</td>
            <td className="to">{c.to === "" ? <span className="muted">(unset)</span> : c.to}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
