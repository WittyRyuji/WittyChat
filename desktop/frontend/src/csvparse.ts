// RFC4180 CSV parser — quotes, escaped "" quotes, CRLF/LF, a BOM. Pure,
// dependency-free; returns rows of string cells.
// RFC4180 解析：引号、内嵌换行、CRLF/LF、BOM；纯函数，无依赖。

export function parseCsv(text: string): string[][] {
  if (text.charCodeAt(0) === 0xfeff) text = text.slice(1); // BOM
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let inQ = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inQ) {
      if (c === '"') {
        if (text[i + 1] === '"') {
          cell += '"';
          i++;
        } else {
          inQ = false;
        }
      } else cell += c;
      continue;
    }
    if (c === '"') {
      inQ = true;
    } else if (c === ",") {
      row.push(cell);
      cell = "";
    } else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++; // CRLF
      row.push(cell);
      cell = "";
      rows.push(row);
      row = [];
    } else cell += c;
  }
  if (cell !== "" || row.length) {
    row.push(cell);
    rows.push(row);
  }
  return rows;
}

/** rows → {r:c} cell map (0-indexed, SheetEditor's ydoc.getMap("cells") shape). */
export function rowsToCells(rows: string[][]): Record<string, string> {
  const cells: Record<string, string> = {};
  for (let r = 0; r < rows.length; r++) {
    for (let c = 0; c < rows[r].length; c++) {
      if (rows[r][c] !== "") cells[`${r}:${c}`] = rows[r][c];
    }
  }
  return cells;
}

/** A1 → "r:c" (A1 is 1-indexed col letters + row number). */
export function a1ToCellRef(a1: string): string {
  const m = /^([A-Za-z]+)(\d+)$/.exec(a1);
  if (!m) return a1;
  let col = 0;
  for (const ch of m[1].toUpperCase()) col = col * 26 + (ch.charCodeAt(0) - 64);
  return `${Number.parseInt(m[2], 10) - 1}:${col - 1}`;
}

/** column letters → 0-indexed index (A→0, B→1, AA→26). */
export function colLettersToIndex(letters: string): number {
  let col = 0;
  for (const ch of letters.toUpperCase()) col = col * 26 + (ch.charCodeAt(0) - 64);
  return col - 1;
}
