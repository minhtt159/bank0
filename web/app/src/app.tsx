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
        <Route path="/transfer" component={() => <Protected><Transfer /></Protected>} />
        <Route path="/transfer/:id" component={(p: { id: string }) => <Protected><Receipt id={p.id} /></Protected>} />
        <Route default component={() => <Protected><Home /></Protected>} />
      </Router>
    </LocationProvider>
  );
}
