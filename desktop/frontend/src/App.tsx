import { useEffect, useState } from "react";
import { api, getToken, setToken, type UserPublic } from "./api";
import { Chat } from "./Chat";
import { Login } from "./Login";

export default function App() {
  const [user, setUser] = useState<UserPublic | null>(null);
  const [booting, setBooting] = useState(!!getToken());

  useEffect(() => {
    if (!getToken()) return;
    api
      .me()
      .then((r) => setUser(r.user))
      .catch(() => setToken(""))
      .finally(() => setBooting(false));
  }, []);

  if (booting) return <div className="boot">WittyChat…</div>;
  if (!user) return <Login onLogin={setUser} />;
  return <Chat me={user} onLogout={() => setUser(null)} />;
}
