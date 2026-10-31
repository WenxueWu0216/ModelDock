import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { API_BASE } from "./api";
import { STREAM_EVENT_TYPES, type StreamMessage } from "./types";

export type LiveState = "connecting" | "live" | "reconnecting";
type Listener = (m: StreamMessage) => void;

interface LiveApi {
  state: LiveState;
  /** Bumped on every (re)connect; pages refetch state when it changes. */
  epoch: number;
  subscribe: (fn: Listener) => () => void;
}

const LiveContext = createContext<LiveApi | null>(null);

/** Factory so tests can inject a fake EventSource. */
export type EventSourceFactory = (url: string) => EventSource;
const defaultFactory: EventSourceFactory = (url) => new EventSource(url);

function streamURL(token: string, lastSeq: number): string {
  const q = new URLSearchParams({ access_token: token });
  if (lastSeq > 0) q.set("lastEventId", String(lastSeq));
  return `${API_BASE}/api/v1/events/stream?${q.toString()}`;
}

/**
 * One EventSource for the whole console. The browser reconnects by itself
 * (sending Last-Event-ID); if it gives up (CLOSED), we reopen with
 * ?lastEventId= after a delay. Events are de-duplicated by seq.
 */
export function LiveProvider({
  token,
  children,
  factory = defaultFactory,
  retryMs = 3000,
}: {
  token: string;
  children: ReactNode;
  factory?: EventSourceFactory;
  retryMs?: number;
}) {
  const [state, setState] = useState<LiveState>("connecting");
  const [epoch, setEpoch] = useState(0);
  const listeners = useRef(new Set<Listener>());
  const lastSeq = useRef(0);

  useEffect(() => {
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;

    const onEvent = (e: MessageEvent<string>) => {
      let m: StreamMessage;
      try {
        m = JSON.parse(e.data) as StreamMessage;
      } catch {
        return;
      }
      if (m.seq <= lastSeq.current) return; // duplicate after a resume
      lastSeq.current = m.seq;
      listeners.current.forEach((fn) => fn(m));
    };

    const open = () => {
      if (stopped) return;
      es = factory(streamURL(token, lastSeq.current));
      es.onopen = () => {
        setState("live");
        setEpoch((n) => n + 1);
      };
      es.onerror = () => {
        if (!es) return;
        setState("reconnecting");
        if (es.readyState === 2 /* CLOSED */) {
          es.close();
          timer = setTimeout(open, retryMs);
        }
      };
      for (const t of STREAM_EVENT_TYPES) es.addEventListener(t, onEvent as EventListener);
      es.addEventListener("stream.reset", () => setEpoch((n) => n + 1));
    };
    open();
    return () => {
      stopped = true;
      clearTimeout(timer);
      es?.close();
    };
  }, [token, factory, retryMs]);

  const api = useRef<Omit<LiveApi, "state" | "epoch">>({
    subscribe: (fn) => {
      listeners.current.add(fn);
      return () => listeners.current.delete(fn);
    },
  });

  return <LiveContext.Provider value={{ state, epoch, subscribe: api.current.subscribe }}>{children}</LiveContext.Provider>;
}

export function useLive(): LiveApi {
  const ctx = useContext(LiveContext);
  if (!ctx) throw new Error("useLive outside LiveProvider");
  return ctx;
}

/** Calls fn for every live event (latest fn, stable subscription). */
export function useLiveEvents(fn: Listener): void {
  const { subscribe } = useLive();
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => subscribe((m) => ref.current(m)), [subscribe]);
}
