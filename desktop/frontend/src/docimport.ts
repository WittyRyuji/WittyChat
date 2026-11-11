// File → import-payload converters for the staged doc import.
// docx/md/html/txt → HTML (doc); xlsx/csv → {name,cells} sheets.
// docx images + html data: images upload as "doc" objects + link via
// /docs/{id}/objects, rewritten to wcobj:<id> for the editor NodeView.
import DOMPurify from "dompurify";
import mammoth from "mammoth";
import { marked } from "marked";
import readXlsxFile, { readSheetNames } from "read-excel-file";
import { api2 } from "./api";
import { parseCsv } from "./csvparse";

const DOC_EXTS = ["docx", "md", "markdown", "html", "htm", "txt"];

// wcobj:<id> must survive DOMPurify or every imported image loses its src.
const WCOBJ_URI = /^(?:(?:https?|mailto|tel|wcobj):|[^a-z]|[a-z+.-]+(?:[^a-z+.\-:]|$))/i;
export function sanitizeImportHtml(html: string): string {
  return DOMPurify.sanitize(html, { ALLOWED_URI_REGEXP: WCOBJ_URI });
}
const SHEET_EXTS = ["xlsx", "csv"];

export function extOf(name: string): string {
  return name.split(".").pop()?.toLowerCase() ?? "";
}
export function isDocFile(name: string): boolean {
  return DOC_EXTS.includes(extOf(name));
}
export function isSheetFile(name: string): boolean {
  return SHEET_EXTS.includes(extOf(name));
}

async function uploadAndLink(blob: Blob, docId: string, name: string): Promise<string> {
  const file = new File([blob], name || "image", { type: blob.type || "image/png" });
  const o = await api2.uploadObject(file, "doc");
  await api2.docObjects(docId, [o.id]);
  return `wcobj:${o.id}`;
}

// rewrite every <img> with a data: src → upload → wcobj:, remote stays
async function rewriteDataImages(html: string, docId: string): Promise<string> {
  const doc = new DOMParser().parseFromString(html, "text/html");
  const imgs = [...doc.querySelectorAll("img[src^='data:']")];
  for (const img of imgs) {
    const res = await fetch(img.getAttribute("src") ?? "");
    const blob = await res.blob();
    const src = await uploadAndLink(blob, docId, "img");
    img.setAttribute("src", src);
  }
  return doc.body.innerHTML;
}

/** blob → sanitized HTML payload for the doc editor. */
export async function fileToHtml(
  blob: Blob,
  format: string,
  opts: { docId: string }
): Promise<{ html: string; truncated: boolean }> {
  switch (format) {
    case "docx": {
      const arrayBuffer = await blob.arrayBuffer();
      const { value } = await mammoth.convertToHtml(
        { arrayBuffer },
        {
          convertImage: mammoth.images.imgElement(async (image) => {
            const b = await image.read("base64");
            const bin = atob(b);
            const bytes = new Uint8Array(bin.length);
            for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
            const src = await uploadAndLink(
              new Blob([bytes], { type: image.contentType }),
              opts.docId,
              "img"
            );
            return { src };
          }),
        }
      );
      return { html: sanitizeImportHtml(value), truncated: false };
    }
    case "md":
    case "markdown": {
      const text = await blob.text();
      const html = await marked.parse(text);
      return {
        html: sanitizeImportHtml(await rewriteDataImages(String(html), opts.docId)),
        truncated: false,
      };
    }
    case "html":
    case "htm": {
      const text = await blob.text();
      return {
        html: sanitizeImportHtml(await rewriteDataImages(text, opts.docId)),
        truncated: false,
      };
    }
    case "txt": {
      const text = await blob.text();
      const esc = (s: string) =>
        s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
      const html = text
        .split(/\r?\n/)
        .map((l) => `<p>${esc(l)}</p>`)
        .join("");
      return { html, truncated: false };
    }
    default:
      throw new Error(`unsupported doc format: ${format}`);
  }
}

const MAX_ROWS = 10_000;
const MAX_COLS = 200;

export interface SheetData {
  name: string;
  cells: Record<string, string>;
  truncated: boolean;
}

/** blob → worksheets for the sheet editor. xlsx: all worksheets; csv: one. */
export async function fileToSheets(blob: Blob, format: string): Promise<SheetData[]> {
  const cap = (rows: string[][]): { cells: Record<string, string>; truncated: boolean } => {
    let truncated = false;
    if (rows.length > MAX_ROWS) {
      rows = rows.slice(0, MAX_ROWS);
      truncated = true;
    }
    const cells: Record<string, string> = {};
    for (let r = 0; r < rows.length; r++) {
      const cols = Math.min(rows[r].length, MAX_COLS);
      if (rows[r].length > MAX_COLS) truncated = true;
      for (let c = 0; c < cols; c++) {
        if (rows[r][c] !== "") cells[`${r}:${c}`] = rows[r][c];
      }
    }
    return { cells, truncated };
  };
  if (format === "xlsx") {
    const names = await readSheetNames(blob);
    const out: SheetData[] = [];
    for (const name of names) {
      const rows = await readXlsxFile(blob, { sheet: name });
      const strs = rows.map((row) =>
        row.map((v) => (v instanceof Date ? v.toISOString() : v == null ? "" : String(v)))
      );
      const { cells, truncated } = cap(strs);
      out.push({ name, cells, truncated });
    }
    return out;
  }
  if (format === "csv") {
    const rows = parseCsv(await blob.text());
    const { cells, truncated } = cap(rows);
    return [{ name: "Sheet1", cells, truncated }];
  }
  throw new Error(`unsupported sheet format: ${format}`);
}
