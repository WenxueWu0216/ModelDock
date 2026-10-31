import type { ReactNode } from "react";
import { vi } from "vitest";
import { AuthProvider, type Session } from "../auth";
import { LiveProvider, type EventSourceFactory } from "../live";
import type { Role, StreamMessage } from "../types";

/** Minimal EventSource double: tests drive open/error/events by hand. */
export class FakeEventSource {
  static instances: FakeEventSource[] = [];
  readyState = 0;
  onopen: ((e: Event) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  private listeners = new Map<string, Set<(e: MessageEvent<string>) => void>>();
  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (e: MessageEvent<string>) => void) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)!.add(fn);
  }
  removeEventListener(type: string, fn: (e: MessageEvent<string>) => void) {
    this.listeners.get(type)?.delete(fn);
  }
  close() {
    this.readyState = 2;
  }
  open() {
    this.readyState = 1;
    this.onopen?.(new Event("open"));
  }
  fail(closed = false) {
    this.readyState = closed ? 2 : 0;
    this.onerror?.(new Event("error"));
  }
  emit(m: Partial<StreamMessage> & { seq: number; type: string }) {
    const full: StreamMessage = { eventId: `e${m.seq}`, actor: "system:reconciler", occurredAt: new Date().toISOString(), ...m };
    const ev = new MessageEvent<string>(m.type, { data: JSON.stringify(full), lastEventId: String(m.seq) });
    this.listeners.get(m.type)?.forEach((fn) => fn(ev));
  }
  static last(): FakeEventSource {
    const es = FakeEventSource.instances.at(-1);
    if (!es) throw new Error("no EventSource opened");
    return es;
  }
}

export const fakeFactory: EventSourceFactory = (url) => new FakeEventSource(url) as unknown as EventSource;

export function session(sub: string, role: Role): Session {
  return { token: `tok-${sub}-${role}`, sub, role };
}

export function Providers({ s, children }: { s: Session; children: ReactNode }) {
  return (
    <AuthProvider initialSession={s}>
      <LiveProvider token={s.token} factory={fakeFactory} retryMs={10}>
        {children}
      </LiveProvider>
    </AuthProvider>
  );
}

type Handler = (init: RequestInit | undefined, url: string) => { status?: number; body: unknown };

/** Routes fetch calls by "METHOD path" (path without query) to handlers. */
export function mockFetch(routes: Record<string, Handler | { status?: number; body: unknown }>) {
  const calls: { method: string; url: string; body?: unknown }[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const path = url.replace(/^https?:\/\/[^/]+/, "");
    calls.push({ method, url: path, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const key = `${method} ${path.split("?")[0]}`;
    const r = routes[key];
    if (!r) return new Response(JSON.stringify({ error: `no mock for ${key}`, code: 404 }), { status: 404 });
    const res = typeof r === "function" ? r(init, path) : r;
    return new Response(JSON.stringify(res.body), { status: res.status ?? 200, headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fn);
  return { fn, calls };
}
