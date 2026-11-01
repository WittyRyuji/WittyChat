// Spreadsheet cell model + formula engine for collaborative sheets.
// Cells live in a Yjs Y.Map keyed "row:col" → raw string; formulas start
// with "=". evalSheet returns display values with dependency resolution,
// circular-ref detection (#CIRC) and typed errors.
//
// 表格单元格模型与公式引擎：单元格存于 Y.Map（"行:列" → 原始串，公式以
// "=" 开头）。evalSheet 解析依赖并返回显示值，支持循环检测与错误码。
import type * as Y from "yjs";

export const MAX_ROWS = 100;
export const MAX_COLS = 26;

export const colName = (c: number): string => String.fromCharCode(65 + c); // A..Z
export const cellKey = (r: number, c: number): string => `${r}:${c}`;

/** Raw cells as a plain map. */
export type RawCells = Record<string, string>;

export function cellsFromMap(m: Y.Map<string>): RawCells {
  const out: RawCells = {};
  for (const [k, v] of m.entries()) out[k] = v;
  return out;
}

/* ---------------- formula parser (Pratt) ---------------- */

type Tok =
  | { t: "num"; v: number }
  | { t: "str"; v: string }
  | { t: "ref"; r: number; c: number }
  | { t: "op"; v: string }
  | { t: "fn"; v: string };

function tokenize(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  const isCol = (ch: string) => ch >= "A" && ch <= "Z";
  while (i < src.length) {
    const ch = src[i];
    if (ch === " " || ch === "\t") {
      i++;
      continue;
    }
    if (ch === '"') {
      let s = "";
      i++;
      while (i < src.length && src[i] !== '"') s += src[i++];
      i++;
      toks.push({ t: "str", v: s });
      continue;
    }
    if (ch >= "0" && ch <= "9") {
      let n = "";
      while (i < src.length && /[\d.]/.test(src[i])) n += src[i++];
      toks.push({ t: "num", v: parseFloat(n) });
      continue;
    }
    if (isCol(ch)) {
      // cell ref A1..Z99 or function name
      let id = "";
      while (i < src.length && /[A-Z_]/.test(src[i])) id += src[i++];
      let num = "";
      while (i < src.length && /\d/.test(src[i])) num += src[i++];
      if (id.length === 1 && num.length > 0) {
        toks.push({ t: "ref", r: parseInt(num, 10) - 1, c: id.charCodeAt(0) - 65 });
      } else {
        toks.push({ t: "fn", v: id });
      }
      continue;
    }
    // operators incl. 2-char
    const two = src.slice(i, i + 2);
    if (two === "<=" || two === ">=" || two === "<>") {
      toks.push({ t: "op", v: two });
      i += 2;
      continue;
    }
    if ("+-*/%^(),:=<>".includes(ch)) {
      toks.push({ t: "op", v: ch });
      i++;
      continue;
    }
    if ("&".includes(ch)) {
      toks.push({ t: "op", v: ch });
      i++;
      continue;
    }
    if (ch === "!") {
      // skip sheet-qualified refs (A1!B2 not supported) — treat as unary not
      i++;
      continue;
    }
    throw new Error(`bad char ${ch}`);
  }
  return toks;
}

interface Range {
  r1: number;
  c1: number;
  r2: number;
  c2: number;
}
class RangeMarker {
  constructor(public idx: number) {}
}
type Val = number | string | boolean | RangeMarker;

type Ctx = {
  getNum: (r: number, c: number) => number;
  getStr: (r: number, c: number) => Val;
};

const num = (v: Val): number => {
  if (typeof v === "number") return v;
  if (v === "") return 0;
  const n = parseFloat(v as string);
  return Number.isNaN(n) ? 0 : n;
};
const str = (v: Val): string => (typeof v === "boolean" ? (v ? "TRUE" : "FALSE") : String(v));
const bool = (v: Val): boolean => (typeof v === "boolean" ? v : v !== 0 && v !== "");

function evalTokens(toks: Tok[], ctx: Ctx): Val {
  let pos = 0;
  const peek = () => toks[pos];
  const next = () => toks[pos++];
  const isOp = (v: string) => {
    const t = peek();
    return t?.t === "op" && t.v === v;
  };
  const eatOp = (v: string) => {
    if (isOp(v)) {
      pos++;
      return true;
    }
    return false;
  };

  function expr(): Val {
    return cmp();
  }
  function cmp(): Val {
    const l = concat();
    const t = peek();
    if (t?.t === "op" && ["=", "<>", "<", ">", "<=", ">="].includes(t.v)) {
      pos++;
      const r = concat();
      const lo = typeof l === "string" && typeof r === "string" ? l : num(l);
      const ro = typeof l === "string" && typeof r === "string" ? r : num(r);
      switch (t.v) {
        case "=":
          return lo === ro;
        case "<>":
          return lo !== ro;
        case "<":
          return lo < ro;
        case ">":
          return lo > ro;
        case "<=":
          return lo <= ro;
        case ">=":
          return lo >= ro;
      }
    }
    return l;
  }
  function concat(): Val {
    let l = add();
    while (isOp("&")) {
      pos++;
      l = str(l) + str(add());
    }
    return l;
  }
  function add(): Val {
    let l = mul();
    for (;;) {
      if (eatOp("+")) l = num(l) + num(mul());
      else if (eatOp("-")) l = num(l) - num(mul());
      else return l;
    }
  }
  function mul(): Val {
    let l = unary();
    for (;;) {
      if (eatOp("*")) l = num(l) * num(unary());
      else if (eatOp("/")) {
        const d = num(unary());
        if (d === 0) throw new Error("#DIV0");
        l = num(l) / d;
      } else if (eatOp("%")) l = num(l) / 100;
      else return l;
    }
  }
  function unary(): Val {
    if (eatOp("-")) return -num(unary());
    eatOp("+");
    return pow();
  }
  function pow(): Val {
    const b = prim();
    if (eatOp("^")) return num(b) ** num(unary());
    return b;
  }
  function rangeVals(rg: Range): Val[] {
    const out: Val[] = [];
    for (let r = Math.min(rg.r1, rg.r2); r <= Math.max(rg.r1, rg.r2); r++)
      for (let c = Math.min(rg.c1, rg.c2); c <= Math.max(rg.c1, rg.c2); c++)
        out.push(ctx.getStr(r, c));
    return out;
  }
  function prim(): Val {
    const t = next();
    if (!t) throw new Error("#ERR");
    if (t.t === "num") return t.v;
    if (t.t === "str") return t.v;
    if (t.t === "ref") return ctx.getStr(t.r, t.c);
    if (t.t === "fn") {
      const name = t.v;
      if (name === "TRUE") return true;
      if (name === "FALSE") return false;
      if (!eatOp("(")) throw new Error("#NAME?");
      // collect args — each may be expr or range (ref:ref)
      const args: Val[] = [];
      const rangeLists: Val[][] = [];
      for (;;) {
        const save = pos;
        const f = next();
        if (f?.t === "ref" && isOp(":")) {
          pos++;
          const s = next();
          if (s?.t === "ref") {
            rangeLists.push(rangeVals({ r1: f.r, c1: f.c, r2: s.r, c2: s.c }));
            args.push(new RangeMarker(rangeLists.length - 1));
          } else {
            pos = save;
            args.push(expr());
          }
        } else {
          pos = save;
          args.push(expr());
        }
        if (eatOp(",")) continue;
        break;
      }
      eatOp(")");
      return callFn(name, args, rangeLists);
    }
    if (t.t === "op" && t.v === "(") {
      const v = expr();
      eatOp(")");
      return v;
    }
    throw new Error("#ERR");
  }
  return expr();
}

function callFn(name: string, args: Val[], rangeLists: Val[][]): Val {
  const nums = (list: Val[]): number[] =>
    list
      .filter(
        (v) =>
          typeof v === "number" ||
          (typeof v === "string" && v !== "" && !Number.isNaN(parseFloat(v)))
      )
      .map(num);
  const flat = (): Val[] => {
    const out: Val[] = [];
    for (const a of args) {
      if (a instanceof RangeMarker) out.push(...rangeLists[a.idx]);
      else out.push(a);
    }
    return out;
  };
  switch (name) {
    case "SUM": {
      let s = 0;
      for (const v of flat()) s += typeof v === "number" ? v : num(v);
      return s;
    }
    case "AVERAGE":
    case "AVG": {
      const vs = nums(flat());
      return vs.length ? vs.reduce((a, b) => a + b, 0) / vs.length : 0;
    }
    case "MIN":
      return Math.min(...nums(flat()));
    case "MAX":
      return Math.max(...nums(flat()));
    case "COUNT":
      return flat().filter(
        (v) =>
          typeof v === "number" ||
          (typeof v === "string" && v !== "" && !Number.isNaN(parseFloat(v)))
      ).length;
    case "COUNTA":
      return flat().filter((v) => v !== "" && v != null).length;
    case "ABS":
      return Math.abs(num(args[0]));
    case "ROUND":
      return Math.round(num(args[0]) * 10 ** num(args[1] ?? 0)) / 10 ** num(args[1] ?? 0);
    case "INT":
      return Math.floor(num(args[0]));
    case "MOD": {
      const d = num(args[1]);
      if (d === 0) throw new Error("#DIV0");
      return num(args[0]) % d;
    }
    case "POWER":
      return num(args[0]) ** num(args[1]);
    case "SQRT":
      return Math.sqrt(num(args[0]));
    case "IF":
      return bool(args[0]) ? (args[1] ?? true) : (args[2] ?? false);
    case "AND":
      return flat().every(bool);
    case "OR":
      return flat().some(bool);
    case "NOT":
      return !bool(args[0]);
    case "CONCAT":
    case "CONCATENATE":
      return flat().map(str).join("");
    case "LEN":
      return str(args[0]).length;
    case "UPPER":
      return str(args[0]).toUpperCase();
    case "LOWER":
      return str(args[0]).toLowerCase();
    case "TRIM":
      return str(args[0]).trim();
    default:
      throw new Error("#NAME?");
  }
}

/* ---------------- sheet evaluation ---------------- */

export interface EvalResult {
  /** key -> display string */
  display: Record<string, string>;
  /** key -> error code (e.g. "#DIV0") */
  errors: Record<string, string>;
}

export function evalSheet(cells: RawCells): EvalResult {
  const display: Record<string, string> = {};
  const errors: Record<string, string> = {};
  const memo = new Map<string, Val>();
  const visiting = new Set<string>();

  const fmt = (v: Val): string => {
    if (typeof v === "number") {
      if (!Number.isFinite(v)) return "#ERR";
      const r = Math.round(v * 1e10) / 1e10;
      return String(r);
    }
    return str(v);
  };

  const getVal = (r: number, c: number): Val => {
    const key = cellKey(r, c);
    const hit = memo.get(key);
    if (hit !== undefined) return hit;
    if (visiting.has(key)) throw new Error("#CIRC");
    const raw = cells[key] ?? "";
    if (raw.startsWith("=")) {
      visiting.add(key);
      try {
        const v = evalTokens(tokenize(raw.slice(1)), {
          getNum: (rr, cc) => num(getVal(rr, cc)),
          getStr: getVal,
        });
        visiting.delete(key);
        memo.set(key, v);
        return v;
      } catch (e) {
        visiting.delete(key);
        throw e;
      }
    }
    const v: Val = raw === "" ? "" : raw;
    memo.set(key, v);
    return v;
  };

  for (const [key, raw] of Object.entries(cells)) {
    if (raw.startsWith("=")) {
      try {
        const [r, c] = key.split(":").map((x) => parseInt(x, 10));
        display[key] = fmt(getVal(r, c));
      } catch (e) {
        const code = e instanceof Error && e.message.startsWith("#") ? e.message : "#ERR";
        display[key] = code;
        errors[key] = code;
      }
    } else {
      display[key] = raw;
    }
  }
  return { display, errors };
}
