import { useState, type FormEvent } from "react";
import { useAuth } from "../auth";

export function Login() {
  const { login } = useAuth();
  const [token, setToken] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    try {
      login(token);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
    }
  };
  return (
    <main className="login">
      <h1>ModelDock</h1>
      <form onSubmit={submit} className="card">
        <label htmlFor="token">Paste a JWT</label>
        <textarea
          id="token"
          rows={5}
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder="eyJhbGciOiJIUzI1NiIs…"
          spellCheck={false}
        />
        <p className="muted small">
          Dev tokens: <code>modeldock token -sub alice -role approver</code>. The token stays in this tab
          (sessionStorage) and is sent only to the ModelDock API.
        </p>
        {err && <p className="error">{err}</p>}
        <button type="submit" disabled={!token.trim()}>
          Sign in
        </button>
      </form>
    </main>
  );
}
