import { describe, expect, it } from "vitest";
import { a1ToCellRef, colLettersToIndex, parseCsv, rowsToCells } from "./csvparse";

describe("parseCsv", () => {
  it("plain rows", () => {
    expect(parseCsv("a,b,c\n1,2,3")).toEqual([
      ["a", "b", "c"],
      ["1", "2", "3"],
    ]);
  });
  it("quoted commas + embedded newline + escaped quotes", () => {
    expect(parseCsv('"x,y","he said ""hi""","l1\nl2"')).toEqual([
      ["x,y", 'he said "hi"', "l1\nl2"],
    ]);
  });
  it("BOM + CRLF", () => {
    expect(parseCsv("﻿a,b\r\n1,2\r\n")).toEqual([
      ["a", "b"],
      ["1", "2"],
    ]);
  });
});
describe("cell refs", () => {
  it("rows → r:c map skips empties", () => {
    expect(
      rowsToCells([
        ["a", "", "c"],
        ["d", "e", ""],
      ])
    ).toEqual({ "0:0": "a", "0:2": "c", "1:0": "d", "1:1": "e" });
  });
  it("A1 → r:c", () => {
    expect(a1ToCellRef("A1")).toBe("0:0");
    expect(a1ToCellRef("B3")).toBe("2:1");
    expect(a1ToCellRef("AA2")).toBe("1:26");
  });
  it("col letters → index", () => {
    expect(colLettersToIndex("A")).toBe(0);
    expect(colLettersToIndex("Z")).toBe(25);
    expect(colLettersToIndex("AA")).toBe(26);
  });
});
