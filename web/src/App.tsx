import { AuthProvider, useAuth, useSession } from "./auth";
import { LiveIndicator } from "./components/LiveIndicator";
import { LiveProvider } from "./live";
import { Approvals } from "./pages/Approvals";
import { Diff } from "./pages/Diff";
import { Login } from "./pages/Login";
import { ModelPage, Models } from "./pages/Models";
import { Timeline } from "./pages/Timeline";
import { Link, useRoute, type Route } from "./router";

const NAV = [
  { to: "/", label: "Models" },
  { to: "/approvals", label: "Approvals" },
  { to: "/timeline", label: "Timeline" },
  { to: "/diff", label: "Diff" },
];

function Page({ route }: { route: Route }) {
  const p = route.path;
  if (p === "/approvals") return <Approvals />;
  if (p === "/timeline") return <Timeline route={route} />;
  if (p === "/diff") return <Diff route={route} />;
  const m = /^\/models\/([^/]+)$/.exec(p);
  if (m) return <ModelPage name={decodeURIComponent(m[1]!)} />;
  return <Models />;
}

function Shell() {
  const route = useRoute();
  const session = useSession();
  const { logout } = useAuth();
  return (
    <LiveProvider token={session.token}>
      <header className="topbar">
        <span className="brand">ModelDock</span>
        <nav>
          {NAV.map((n) => (
            <Link key={n.to} to={n.to} className={route.path === n.to ? "active" : ""}>
              {n.label}
            </Link>
          ))}
        </nav>
        <span className="spacer" />
        <LiveIndicator />
        <span className="user" title={session.exp ? `token expires ${new Date(session.exp * 1000).toLocaleString()}` : undefined}>
          {session.name || session.sub} <span className="role">{session.role}</span>
        </span>
        <button className="link" onClick={logout}>
          Sign out
        </button>
      </header>
      <main className="content">
        <Page route={route} />
      </main>
    </LiveProvider>
  );
}

function Gate() {
  const { session } = useAuth();
  return session ? <Shell /> : <Login />;
}

export function App() {
  return (
    <AuthProvider>
      <Gate />
    </AuthProvider>
  );
}
