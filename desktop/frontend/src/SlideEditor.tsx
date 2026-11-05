// Collaborative slide deck: Y.Array of Y.Map{title,body}. Left thumbnail
// rail for navigation; main canvas edits title + body. Peer focus slide
// goes through awareness. 协同演示：Y.Array 存 Y.Map{标题,正文}，左侧
// 缩略图导航，主画布编辑；对端所在页经 awareness 广播。
import { useCallback, useEffect, useState } from "react";
import type { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";
import type { UserPublic } from "./api";

interface SlideData {
  title: string;
  body: string;
}

export default function SlideEditor({
  ydoc,
  provider,
  me,
  synced,
  isOwner,
}: {
  ydoc: Y.Doc;
  provider: WebsocketProvider;
  me: UserPublic;
  synced: boolean;
  isOwner: boolean;
}) {
  const slides = ydoc.getArray<Y.Map<string>>("slides");
  const [count, setCount] = useState(slides.length);
  // Re-render trigger only — never use it as a React key: remounting the
  // canvas on every change would steal focus from the input being typed in.
  // 仅作重渲染触发；绝不能当 key，否则每次改动重挂载画布会夺走输入焦点。
  const [, setTick] = useState(0);
  const [cur, setCur] = useState(0);
  const [peersOn, setPeersOn] = useState<Record<number, string[]>>({});

  useEffect(() => {
    const obs = () => {
      setCount(slides.length);
      setTick((t) => t + 1);
    };
    slides.observeDeep(obs);
    return () => slides.unobserveDeep(obs);
  }, [slides]);

  // Seed the first slide once synced — owner only, so two members opening
  // an empty deck at once can't both push a "page 1".
  // 同步后补第一页——仅属主执行，避免两人同时打开空演示各插一页。
  useEffect(() => {
    if (synced && isOwner && slides.length === 0) {
      const m = new Y.Map<string>();
      m.set("title", ""); // empty → shown as 第 N 页 placeholder / 空标题显示为占位
      m.set("body", "");
      ydoc.transact(() => slides.push([m]));
    }
  }, [synced, isOwner, slides, ydoc]);

  // peer focus via awareness.user.slide
  useEffect(() => {
    const aw = provider.awareness;
    const upd = () => {
      const mine = aw.clientID;
      const out: Record<number, string[]> = {};
      for (const [cid, st] of aw.getStates() as Map<
        number,
        { user?: { name?: string; slide?: number } }
      >) {
        if (cid === mine) continue;
        const s = st.user?.slide;
        if (s !== undefined && st.user?.name) {
          out[s] = [...(out[s] ?? []), st.user.name];
        }
      }
      setPeersOn(out);
    };
    aw.on("change", upd);
    upd();
    return () => aw.off("change", upd);
  }, [provider]);

  const goto = useCallback(
    (i: number) => {
      setCur(i);
      // Keep the identity/color the pane published; only add our slide.
      // 保留外层发布的身份与颜色，只追加当前页。
      const prev = (provider.awareness.getLocalState()?.user ?? {}) as { name?: string };
      provider.awareness.setLocalStateField("user", {
        ...prev,
        name: prev.name ?? (me.display_name || me.email),
        slide: i,
      });
    },
    [provider, me]
  );

  const addSlide = (after?: number) => {
    ydoc.transact(() => {
      const m = new Y.Map<string>();
      m.set("title", ""); // empty title → 第 N 页 placeholder / 空标题占位显示
      m.set("body", "");
      if (after === undefined) slides.push([m]);
      else slides.insert(after + 1, [m]);
    });
    goto(after === undefined ? slides.length - 1 : after + 1);
  };

  const delSlide = (i: number) => {
    if (slides.length <= 1) return;
    ydoc.transact(() => slides.delete(i, 1));
    setCur(Math.max(0, Math.min(i, slides.length - 1)));
  };

  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= slides.length) return;
    ydoc.transact(() => {
      const m = slides.get(i);
      const snap = new Y.Map<string>();
      snap.set("title", m.get("title") ?? "");
      snap.set("body", m.get("body") ?? "");
      slides.delete(i, 1);
      slides.insert(j, [snap]);
    });
    goto(j);
  };

  const setField = (i: number, f: "title" | "body", v: string) => {
    const m = slides.get(i);
    if (m) ydoc.transact(() => m.set(f, v));
  };

  const curIdx = Math.max(0, Math.min(cur, count - 1));
  const data: SlideData[] = [];
  for (let i = 0; i < count; i++) {
    const m = slides.get(i);
    data.push({ title: m?.get("title") ?? "", body: m?.get("body") ?? "" });
  }

  return (
    <div className="slide-wrap">
      <div className="slide-rail">
        <button type="button" className="ghost slide-add" onClick={() => addSlide()}>
          ＋ 新增页
        </button>
        {data.map((s, i) => (
          <button
            type="button"
            // biome-ignore lint/suspicious/noArrayIndexKey: ordered slides — position IS the identity / 顺序即标识
            key={i}
            className={`slide-thumb${i === curIdx ? " active" : ""}`}
            onClick={() => goto(i)}
          >
            <span className="thumb-num">{i + 1}</span>
            <span className="thumb-title">{s.title || `第 ${i + 1} 页`}</span>
            {peersOn[i]?.length ? (
              <span className="thumb-peers">{peersOn[i].map((n) => n[0]).join("")}</span>
            ) : null}
          </button>
        ))}
      </div>
      <div className="slide-main">
        <div className="slide-ops">
          <button
            type="button"
            className="ghost"
            disabled={curIdx <= 0}
            onClick={() => move(curIdx, -1)}
          >
            ↑ 上移
          </button>
          <button
            type="button"
            className="ghost"
            disabled={curIdx >= count - 1}
            onClick={() => move(curIdx, 1)}
          >
            ↓ 下移
          </button>
          <button type="button" className="ghost" onClick={() => addSlide(curIdx)}>
            ＋ 在此页后插入
          </button>
          <button
            type="button"
            className="ghost danger"
            disabled={count <= 1}
            onClick={() => delSlide(curIdx)}
          >
            ✕ 删除本页
          </button>
          <span className="slide-count">
            {curIdx + 1} / {count}
          </span>
        </div>
        {count === 0 ? (
          <div className="empty">演示还没有页面，点左侧「＋ 新增页」开始</div>
        ) : (
          <div className="slide-canvas">
            <input
              className="slide-title"
              value={data[curIdx]?.title ?? ""}
              placeholder={`第 ${curIdx + 1} 页`}
              onChange={(e) => setField(curIdx, "title", e.target.value)}
            />
            <textarea
              className="slide-body"
              value={data[curIdx]?.body ?? ""}
              placeholder="正文内容…"
              onChange={(e) => setField(curIdx, "body", e.target.value)}
            />
          </div>
        )}
      </div>
    </div>
  );
}
