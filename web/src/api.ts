// Thin fetch wrappers for the REST and GraphQL APIs. Errors carry the HTTP
// status and the server's message so pages can show e.g. the four-eyes 403.

export const API_BASE: string = (import.meta.env.VITE_API_BASE as string | undefined) ?? "";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

type Unauthorized = () => void;
let onUnauthorized: Unauthorized = () => {};

/** Registers the callback run on any 401 (the app logs out). */
export function setUnauthorizedHandler(fn: Unauthorized): void {
  onUnauthorized = fn;
}

let currentToken: string | null = null;

export function setToken(t: string | null): void {
  currentToken = t;
}

function headers(): HeadersInit {
  const h: Record<string, string> = { "Content-Type": "application/json" };
  if (currentToken) h.Authorization = `Bearer ${currentToken}`;
  return h;
}

export async function rest<T>(path: string, init: { method?: string; body?: unknown } = {}): Promise<T> {
  const res = await fetch(API_BASE + path, {
    method: init.method ?? "GET",
    headers: headers(),
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
  const text = await res.text();
  let json: unknown = undefined;
  try {
    json = text ? JSON.parse(text) : undefined;
  } catch {
    /* non-JSON body */
  }
  if (!res.ok) {
    if (res.status === 401) onUnauthorized();
    const msg = (json as { error?: string } | undefined)?.error ?? (text || res.statusText);
    throw new ApiError(res.status, msg);
  }
  return json as T;
}

const gqlStatus: Record<string, number> = {
  UNAUTHENTICATED: 401,
  FORBIDDEN: 403,
  NOT_FOUND: 404,
  CONFLICT: 409,
  BAD_USER_INPUT: 400,
};

/** Runs a GraphQL operation. The first GraphQL error becomes an ApiError. */
export async function gql<T>(query: string, variables: Record<string, unknown> = {}): Promise<T> {
  const res = await fetch(API_BASE + "/graphql", {
    method: "POST",
    headers: headers(),
    body: JSON.stringify({ query, variables }),
  });
  if (res.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "unauthorized");
  }
  const body = (await res.json()) as {
    data?: T;
    errors?: { message: string; extensions?: { code?: string } }[];
  };
  const err = body.errors?.[0];
  if (err) {
    const code = err.extensions?.code;
    const status = (code && gqlStatus[code]) || 500;
    if (status === 401) onUnauthorized();
    throw new ApiError(status, err.message, code);
  }
  return body.data as T;
}

/** Strips the service-layer prefix ("service: decide approval …: ") from messages. */
export function friendlyMessage(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403 && /cannot approve/.test(e.message)) {
      return "Four-eyes rule: you requested this promotion, so someone else must approve it.";
    }
    const parts = e.message.split(": ");
    const tail = parts.slice(-2).join(": ");
    return e.status === 403 ? `Not allowed: ${tail}` : tail;
  }
  return e instanceof Error ? e.message : String(e);
}
