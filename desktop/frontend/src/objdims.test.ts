import { describe, expect, it } from "vitest";
import { DIM_MAX, fitBox, readDim, writeDim } from "./objdims";

// node env: localStorage is absent — the cache must tolerate that
// (in-memory map only, persistence best-effort).
// node 环境无 localStorage——缓存必须容忍（仅用内存 map，落盘尽力而为）。

describe("fitBox", () => {
  it("fits natural dims into the per-context caps", () => {
    expect(fitBox(517, 389, "attstk")).toEqual({ width: 260, height: 196 });
    expect(fitBox(300, 400, "attstk")).toEqual({ width: 150, height: 200 });
    expect(fitBox(400, 300, "attstk")).toEqual({ width: 260, height: 195 });
    // never upscale — a 32×32 keeps its real size
    expect(fitBox(32, 32, "att")).toEqual({ width: 32, height: 32 });
    expect(fitBox(2400, 300, "mail")).toEqual({ width: 320, height: 40 });
  });

  it("returns undefined for nullish/zero dims or unknown ctx", () => {
    expect(fitBox(null, 100, "att")).toBeUndefined();
    expect(fitBox(0, 100, "att")).toBeUndefined();
    expect(fitBox(undefined, undefined, "att")).toBeUndefined();
    expect(fitBox(100, 100, "nope")).toBeUndefined();
  });
});

describe("dim cache", () => {
  it("round-trips a written dim", () => {
    const id = `rt-${crypto.randomUUID()}`;
    writeDim(id, "thumb", "att", 123, 45);
    expect(readDim(id, "thumb", "att")).toEqual({ width: 123, height: 45 });
  });

  it("evicts the oldest entry past DIM_MAX", () => {
    const p = `ev-${crypto.randomUUID()}`;
    writeDim(`${p}-old`, "thumb", "att", 1, 1);
    for (let i = 0; i < DIM_MAX + 1; i++) {
      writeDim(`${p}-${i}`, "thumb", "att", 2, 2);
    }
    expect(readDim(`${p}-old`, "thumb", "att")).toBeUndefined();
    expect(readDim(`${p}-${DIM_MAX}`, "thumb", "att")).toEqual({ width: 2, height: 2 });
  });

  it("re-writing a key refreshes it against eviction", () => {
    const p = `rw-${crypto.randomUUID()}`;
    writeDim(`${p}-keep`, "thumb", "att", 1, 1);
    writeDim(`${p}-victim`, "thumb", "att", 1, 1);
    // New value → re-inserted at the newest position.
    writeDim(`${p}-keep`, "thumb", "att", 9, 9);
    for (let i = 0; i < DIM_MAX - 1; i++) {
      writeDim(`${p}-${i}`, "thumb", "att", 2, 2);
    }
    expect(readDim(`${p}-keep`, "thumb", "att")).toEqual({ width: 9, height: 9 });
    expect(readDim(`${p}-victim`, "thumb", "att")).toBeUndefined();
  });
});
