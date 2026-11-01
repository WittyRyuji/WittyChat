// Collaborative sheet editor: grid cells backed by Y.Map "r:c"→raw string;
// formulas via sheetEval (A1 refs, ranges, SUM/AVG/IF/…). Selection is
// broadcast through awareness so peers see each other's active cell.
//
// 协同表格：单元格存 Y.Map，公式由 sheetEval 解析（A1 引用/区域/常用函数）。
// 当前选中单元格经 awareness 广播，互相可见对方焦点。
import { useCallback, useEffect, useRef, useState } from "react";
import type { WebsocketProvider } from "y-websocket";
import type * as Y from "yjs";
import type { UserPublic } from "./api";
import { cellKey, cellsFromMap, colName, evalSheet, MAX_COLS, MAX_ROWS } from "./sheetEval";

interface AwUser {
  name?: string;
  color?: string;
  cell?: string;
}

export default function SheetEditor({
  ydoc,
  provider,
  me,
}: {
  ydoc: Y.Doc;
  provider: WebsocketProvider;
  me: UserPublic;
}) {
  const cellsMap = ydoc.getMap<string>("cells");
  const [cells, setCells] = useState(() => cellsFromMap(cellsMap));
  const [sel, setSel] = useState<{ r: number; c: number }>({ r: 0, c: 0 });
  const [editing, setEditing] = useState<{ r: number; c: number; v: string } | null>(null);
  const [peerCells, setPeerCells] = useState<Record<string, AwUser>>({});
  const gridRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const obs = () => setCells(cellsFromMap(cellsMap));
    cellsMap.observe(obs);
    return () => cellsMap.unobserve(obs);
  }, [cellsMap]);

  // peer selection via awareness
  useEffect(() => {
    const aw = provider.awareness;
    const upd = () => {
      const mine = aw.clientID;
      const out: Record<string, AwUser> = {};
      for (const [cid, st] of aw.getStates() as Map<number, { user?: AwUser }>) {
        const cell = st.user?.cell;
        if (cid !== mine && cell && st.user) out[cell] = { ...st.user };
      }
      setPeerCells(out);
    };
    aw.on("change", upd);
    upd();
    return () => aw.off("change", upd);
  }, [provider]);

  const ev = evalSheet(cells);
  const selKey = cellKey(sel.r, sel.c);
  const rawVal = editing ? editing.v : (cells[selKey] ?? "");

  const commit = useCallback(
    (r: number, c: number, v: string) => {
      const k = cellKey(r, c);
      const cur = cellsMap.get(k) ?? "";
      if (v === cur) {
        setEditing(null);
        return;
      }
      ydoc.transact(() => {
        if (v === "") cellsMap.delete(k);
        else cellsMap.set(k, v);
      });
      setEditing(null);
    },
    [cellsMap, ydoc]
  );

  const startEdit = useCallback(
    (r: number, c: number, init?: string) => {
      setSel({ r, c });
      setEditing({ r, c, v: init ?? cellsMap.get(cellKey(r, c)) ?? "" });
      setTimeout(() => inputRef.current?.focus(), 0);
    },
    [cellsMap]
  );

  const select = useCallback(
    (r: number, c: number) => {
      const rr = Math.max(0, Math.min(MAX_ROWS - 1, r));
      const cc = Math.max(0, Math.min(MAX_COLS - 1, c));
      setSel({ r: rr, c: cc });
      provider.awareness.setLocalStateField("user", {
        name: me.display_name || me.email,
        color: "#7048e8",
        cell: cellKey(rr, cc),
      });
      const rowEl = gridRef.current?.querySelector(`[data-row="${rr}"]`);
      rowEl?.scrollIntoView({ block: "nearest" });
    },
    [provider, me]
  );

  const onGridKey = (e: React.KeyboardEvent) => {
    if (editing) return;
    const { r, c } = sel;
    switch (e.key) {
      case "ArrowUp":
        select(r - 1, c);
        break;
      case "ArrowDown":
        select(r + 1, c);
        break;
      case "ArrowLeft":
        select(r, c - 1);
        break;
      case "ArrowRight":
        select(r, c + 1);
        break;
      case "Enter":
        startEdit(r, c);
        break;
      case "Tab":
        e.preventDefault();
        select(r, c + 1);
        break;
      case "Delete":
      case "Backspace":
        ydoc.transact(() => cellsMap.delete(selKey));
        break;
      case "F2":
        startEdit(r, c);
        break;
      default:
        if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
          startEdit(r, c, e.key);
        }
        return;
    }
    e.preventDefault();
  };

  const onEditKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!editing) return;
    if (e.key === "Enter") {
      commit(editing.r, editing.c, editing.v);
      select(editing.r + 1, editing.c);
      e.preventDefault();
    } else if (e.key === "Tab") {
      commit(editing.r, editing.c, editing.v);
      select(editing.r, editing.c + 1);
      e.preventDefault();
    } else if (e.key === "Escape") {
      setEditing(null);
      e.preventDefault();
    }
  };

  const rows: number[] = [];
  const usedRows = Object.keys(cells).reduce(
    (m, k) => Math.max(m, parseInt(k.split(":")[0], 10) + 1),
    0
  );
  const showRows = Math.min(MAX_ROWS, Math.max(30, usedRows + 10));
  for (let r = 0; r < showRows; r++) rows.push(r);

  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex lint/a11y/noStaticElementInteractions: spreadsheet grid is keyboard-navigable by design / 表格网格本身用键盘导航
    <div className="sheet-wrap" onKeyDown={onGridKey} tabIndex={0}>
      <div className="sheet-fx">
        <span className="fx-name">
          {colName(sel.c)}
          {sel.r + 1}
        </span>
        <span className="fx-tag">fx</span>
        <input
          ref={inputRef}
          className="fx-input"
          value={rawVal}
          placeholder="输入值或以 = 开头的公式，如 =SUM(A1:A10)"
          onChange={(e) =>
            editing
              ? setEditing({ ...editing, v: e.target.value })
              : setEditing({ r: sel.r, c: sel.c, v: e.target.value })
          }
          onFocus={() => !editing && setEditing({ r: sel.r, c: sel.c, v: cells[selKey] ?? "" })}
          onBlur={() => editing && commit(editing.r, editing.c, editing.v)}
          onKeyDown={onEditKey}
        />
      </div>
      <div className="sheet-grid" ref={gridRef}>
        <table>
          <thead>
            <tr>
              <th className="corner" />
              {Array.from({ length: MAX_COLS }, (_, c) => (
                <th key={colName(c)}>{colName(c)}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r} data-row={r}>
                <th>{r + 1}</th>
                {Array.from({ length: MAX_COLS }, (_, c) => {
                  const k = cellKey(r, c);
                  const isSel = sel.r === r && sel.c === c;
                  const peer = peerCells[k];
                  const disp = ev.display[k] ?? "";
                  const isErr = ev.errors[k] !== undefined;
                  const isEditing = editing?.r === r && editing?.c === c;
                  return (
                    // biome-ignore lint/a11y/useKeyWithClickEvents: cell keyboard nav handled at grid level / 键盘导航在网格局部处理
                    <td
                      key={k}
                      className={`sheet-cell${isSel ? " sel" : ""}${peer ? " peer" : ""}${isErr ? " err" : ""}${isEditing ? " editing" : ""}`}
                      style={
                        peer
                          ? { boxShadow: `inset 0 0 0 2px ${peer.color ?? "#7048e8"}` }
                          : undefined
                      }
                      onClick={() => select(r, c)}
                      onDoubleClick={() => startEdit(r, c)}
                    >
                      {peer && (
                        <span className="peer-tag" style={{ background: peer.color }}>
                          {peer.name?.[0]}
                        </span>
                      )}
                      {isEditing ? (
                        <input
                          className="cell-input"
                          ref={(el) => el?.focus()}
                          value={editing.v}
                          onChange={(e) => setEditing({ ...editing, v: e.target.value })}
                          onBlur={() => commit(editing.r, editing.c, editing.v)}
                          onKeyDown={onEditKey}
                        />
                      ) : (
                        <span
                          className={`cell-val${typeof disp === "string" && /^-?[\d.]+$/.test(disp) ? " num" : ""}`}
                        >
                          {disp}
                        </span>
                      )}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="sheet-hint">
        Enter/F2 编辑 · 方向键移动 · 公式以 = 开头（SUM AVG MIN MAX COUNT IF AND OR ROUND CONCAT…）
      </div>
    </div>
  );
}
