import { LocationProvider, Router, Route, useLocation } from "preact-iso";
import type { ComponentChildren } from "preact";
import { isAuthed } from "./store/auth";
import { api } from "./api/client";
import { Login } from "./routes/Login";
import { Home } from "./routes/Home";
import { Account } from "./routes/Account";
import { Profile } from "./routes/Profile";
import { Transfer } from "./routes/Transfer";
import { Receipt } from "./routes/Receipt";
import { Activity } from "./routes/Activity";
import { Disputes } from "./routes/Disputes";
import { Devices } from "./routes/Devices";
import { ChangePassword } from "./routes/ChangePassword";

// Protected gates every authenticated screen: no token -> bounce to /login.
function Protected({ children }: { children: ComponentChildren }) {
  const { route } = useLocation();
  if (!isAuthed.value) {
    route("/login", true);
    return null;
  }
  return <Shell>{children}</Shell>;
}

function Shell({ children }: { children: ComponentChildren }) {
  const { path, route } = useLocation();
  return (
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="/">bank0</a>
        <nav>
          <a href="/activity" class={path === "/activity" ? "active" : ""}>Activity</a>
          <a href="/profile" class={path === "/profile" ? "active" : ""}>Profile</a>
          <button class="link" onClick={() => api.logout()}>Sign out</button>
        </nav>
      </header>
      <main>{children}</main>
      <button class="fab" title="Send money" onClick={() => route("/transfer")}>+ Send</button>
    </div>
  );
}

export function App() {
  return (
    <LocationProvider>
      <Router>
        <Route path="/login" component={Login} />
        <Route path="/" component={() => <Protected><Home /></Protected>} />
        <Route path="/accounts/:id" component={(p: { id: string }) => <Protected><Account id={p.id} /></Protected>} />
        <Route path="/profile" component={() => <Protected><Profile /></Protected>} />
        <Route path="/password" component={() => <Protected><ChangePassword /></Protected>} />
        <Route path="/devices" component={() => <Protected><Devices /></Protected>} />
        <Route path="/activity" component={() => <Protected><Activity /></Protected>} />
        <Route path="/disputes" component={() => <Protected><Disputes /></Protected>} />
        <Route path="/transfer" component={() => <Protected><Transfer /></Protected>} />
        <Route path="/transfer/:id" component={(p: { id: string }) => <Protected><Receipt id={p.id} /></Protected>} />
        <Route default component={() => <Protected><Home /></Protected>} />
      </Router>
    </LocationProvider>
  );
}
