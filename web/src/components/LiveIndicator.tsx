import { useLive } from "../live";

const label = { connecting: "connecting", live: "live", reconnecting: "reconnecting" } as const;

export function LiveIndicator() {
  const { state } = useLive();
  return (
    <span className={`live live-${state}`} role="status" aria-live="polite" data-testid="live-indicator">
      <span className="dot" aria-hidden="true" />
      {label[state]}
    </span>
  );
}
