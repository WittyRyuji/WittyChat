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
  const [docFull, setDocFull] = useState(false);

  const [{ ydoc, provider }] = useState(() => {
    const y = new Y.Doc();
    const p = new WebsocketProvider(docWsBase(), docId, y, {
      // Token via Sec-WebSocket-Protocol (wt1.<token>), not a URL param —
      // keeps it out of access logs/browser histories. "wittychat" is the
      // benign protocol the server echoes back. token 走
      // Sec-WebSocket-Protocol 子协议而非 URL 参数——不进日志/历史；
      // 服务端回选固定的 "wittychat" 而非 wt1。
      protocols: ["wittychat", `wt1.${getToken()}`],
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
      const states = [...aw.getStates().values()] as { user?: PeerUser; docFull?: boolean }[];
      setPeers(states.map((s) => s.user ?? {}).filter((u) => u.name));
      // The relay marks "log at cap" as a pseudo awareness state —
      // persisted edits are being rejected until the owner compacts.
      // 中继以伪 awareness 状态标记"日志写满"——属主压实前编辑不落库。
      setDocFull(states.some((s) => s.docFull === true));
    };
    aw.on("change", onChange);
    onChange();
    return () => aw.off("change", onChange);
  }, [provider]);

  return { ydoc, provider, peers, connected, synced, docFull };
}
