// Shared live-doc connection: one Y.Doc + one y-websocket provider per
// mounted editor. Exposes synced/status + awareness peers so doc, sheet
// and slide editors all ride the same /ws/doc/{id} relay.
//
// 编辑器共享连接层：每个挂载的编辑器一个 Y.Doc + provider，向上暴露
// 同步状态与在场 peers——文档/表格/演示都走同一 /ws/doc/{id} 中继。
import { useEffect, useState } from "react";
import { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";
import { docWsBase, getToken } from "./api";

export interface PeerUser {
  name?: string;
  color?: string;
}

export function useDocConn(docId: string) {
  const [peers, setPeers] = useState<PeerUser[]>([]);
  const [connected, setConnected] = useState(false);
  const [synced, setSynced] = useState(false);

  const [{ ydoc, provider }] = useState(() => {
    const y = new Y.Doc();
    const p = new WebsocketProvider(docWsBase(), docId, y, {
      params: { token: getToken() },
    });
    return { ydoc: y, provider: p };
  });

  useEffect(
    () => () => {
      provider.destroy();
      ydoc.destroy();
    },
    [provider, ydoc]
  );

  useEffect(() => {
    const onSync = (s: boolean) => setSynced(s);
    const onStatus = ({ status }: { status: string }) => setConnected(status === "connected");
    provider.on("sync", onSync);
    provider.on("status", onStatus);
    return () => {
      provider.off("sync", onSync);
      provider.off("status", onStatus);
    };
  }, [provider]);

  useEffect(() => {
    const aw = provider.awareness;
    const onChange = () => {
      const states = [...aw.getStates().values()] as { user?: PeerUser }[];
      setPeers(states.map((s) => s.user ?? {}).filter((u) => u.name));
    };
    aw.on("change", onChange);
    onChange();
    return () => aw.off("change", onChange);
  }, [provider]);

  return { ydoc, provider, peers, connected, synced };
}
