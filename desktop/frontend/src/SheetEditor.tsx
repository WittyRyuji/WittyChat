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

// One edit session, typed either in the cell or in the fx bar. Focus can
// hand off between the two without committing.
// 一次编辑会话，输入源为单元格或公式栏；两者间切换焦点不提交。
interface Editing {
  r: number;
  c: number;
  v: string;
  src: "cell" | "fx";
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
  const [editing, setEditing] = useState<Editing | null>(null);
  const [peerCells, setPeerCells] = useState<Record<string, AwUser>>({});
  const wrapRef = useRef<HTMLDivElement>(null);
  const gridRef = useRef<HTMLDivElement>(null);
  const fxRef = useRef<HTMLInputElement>(null);
  const cellInputRef = useRef<HTMLInputElement>(null);

  // Focus the in-cell editor once when a cell edit opens (caret at end).
  // 单元格编辑开启时聚焦一次（光标置尾）。
  const editKey = editing ? `${editing.r}:${editing.c}:${editing.src}` : "";
  // biome-ignore lint/correctness/useExhaustiveDependencies: run per edit session only / 每次编辑会话只跑一次
  useEffect(() => {
    const el = cellInputRef.current;
    if (editing?.src === "cell" && el && document.activeElement !== el) {
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    }
  }, [editKey]);

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
      setEditing({ r, c, v: init ?? cellsMap.get(cellKey(r, c)) ?? "", src: "cell" });
    },
    [cellsMap]
  );

  const select = useCallback(
    (r: number, c: number) => {
      const rr = Math.max(0, Math.min(MAX_ROWS - 1, r));
      const cc = Math.max(0, Math.min(MAX_COLS - 1, c));
      setSel({ r: rr, c: cc });
      // Keep the identity/color the pane published; only add our cell.
      // 保留外层发布的身份与颜色，只追加当前单元格。
      const prev = (provider.awareness.getLocalState()?.user ?? {}) as AwUser;
      provider.awareness.setLocalStateField("user", {
        ...prev,
        name: prev.name ?? (me.display_name || me.email),
        cell: cellKey(rr, cc),
      });
      const rowEl = gridRef.current?.querySelector(`[data-row="${rr}"]`);
      rowEl?.scrollIntoView({ block: "nearest" });
    },
    [provider, me]
  );

  // Commit and hand keyboard focus back to the grid so arrows/typing keep
  // working. 提交并把键盘焦点交还网格，方向键/直接输入继续可用。
  const finish = (ed: Editing, move?: [number, number]) => {
    commit(ed.r, ed.c, ed.v);
    if (move) select(move[0], move[1]);
    wrapRef.current?.focus();
  };

  // Blur commits unless focus is merely moving between cell editor and fx.
  // 失焦即提交，除非焦点只是在单元格编辑框与公式栏之间切换。
  const onEditBlur = (e: React.FocusEvent<HTMLInputElement>) => {
    const to = e.relatedTarget;
    if (!editing || to === fxRef.current || to === cellInputRef.current) return;
    commit(editing.r, editing.c, editing.v);
  };

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
          // The char is seeded via startEdit — prevent the browser from
          // also inserting it into the freshly-mounted input (double char).
          // 首字符由 startEdit 预填；阻止浏览器再把它敲进新出现的输入框。
          e.preventDefault();
          startEdit(r, c, e.key);
        }
        return;
    }
    e.preventDefault();
  };

  const onEditKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    e.stopPropagation(); // keep typing out of the grid handler / 不让网格处理输入键
    if (!editing) return;
    if (e.key === "Enter") {
      e.preventDefault();
      finish(editing, [editing.r + 1, editing.c]);
    } else if (e.key === "Tab") {
      e.preventDefault();
      finish(editing, [editing.r, editing.c + 1]);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setEditing(null);
      wrapRef.current?.focus();
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
    <div className="sheet-wrap" ref={wrapRef} onKeyDown={onGridKey} tabIndex={0}>
      <div className="sheet-fx">
        <span className="fx-name">
          {colName(sel.c)}
          {sel.r + 1}
        </span>
        <span className="fx-tag">fx</span>
        <input
          ref={fxRef}
          className="fx-input"
          value={rawVal}
          placeholder="输入值或以 = 开头的公式，如 =SUM(A1:A10)"
          onChange={(e) =>
            setEditing(
              editing
                ? { ...editing, v: e.target.value, src: "fx" }
                : { r: sel.r, c: sel.c, v: e.target.value, src: "fx" }
            )
          }
          onFocus={() =>
            setEditing(
              editing
                ? { ...editing, src: "fx" }
                : { r: sel.r, c: sel.c, v: cells[selKey] ?? "", src: "fx" }
            )
          }
          onBlur={onEditBlur}
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
                  const inCell = isEditing && editing?.src === "cell";
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
                      {inCell && editing ? (
                        <input
                          className="cell-input"
                          ref={cellInputRef}
                          value={editing.v}
                          onChange={(e) => setEditing({ ...editing, v: e.target.value })}
                          onBlur={onEditBlur}
                          onKeyDown={onEditKey}
                        />
                      ) : (
                        <span className={`cell-val${/^-?[\d.]+$/.test(disp) ? " num" : ""}`}>
                          {isEditing && editing ? editing.v : disp}
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
