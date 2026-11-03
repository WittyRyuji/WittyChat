import { describe, expect, it } from "vitest";
import { evalSheet } from "./sheetEval";

// helper: evaluate a single formula against fixed inputs (A1=10, B1=9, C1="abc")
const run = (formula: string, extra: Record<string, string> = {}) =>
  evalSheet({ "0:0": "10", "0:1": "9", "0:2": "abc", "5:5": formula, ...extra }).display["5:5"];

describe("sheetEval", () => {
  it("does arithmetic with precedence and refs", () => {
    expect(run("=A1+B1*2")).toBe("28");
    expect(run("=(A1+B1)*2")).toBe("38");
    expect(run("=2^3")).toBe("8");
    expect(run("=-A1+1")).toBe("-9");
    expect(run("=50%")).toBe("0.5");
  });

  it("compares typed numbers numerically, not as text", () => {
    expect(run("=A1>B1")).toBe("TRUE"); // 10 > 9 ("10" < "9" as text)
    expect(run('=IF(A1>B1,"A","B")')).toBe("A");
    expect(run("=B1>=A1")).toBe("FALSE");
    expect(run("=A1=10")).toBe("TRUE");
  });

  it("compares text case-insensitively", () => {
    expect(run('=C1="ABC"')).toBe("TRUE");
    expect(run('=C1<>"abd"')).toBe("TRUE");
  });

  it("accepts lowercase refs and function names", () => {
    expect(run("=a1+b1")).toBe("19");
    expect(run("=sum(a1:b1)")).toBe("19");
  });

  it("aggregates ranges", () => {
    expect(run("=SUM(A1:B1)")).toBe("19");
    expect(run("=SUM(A1:B1,1)")).toBe("20");
    expect(run("=AVERAGE(A1:B1)")).toBe("9.5");
    expect(run("=MIN(A1:B1)")).toBe("9");
    expect(run("=MAX(A1:B1)")).toBe("10");
    expect(run("=COUNT(A1:C1)")).toBe("2"); // "abc" is not a number
    expect(run("=COUNTA(A1:C1)")).toBe("3");
  });

  it("supports text/logic functions", () => {
    expect(run('=CONCAT("a",A1)')).toBe("a10");
    expect(run('="x"&B1')).toBe("x9");
    expect(run("=AND(A1>1,B1>1)")).toBe("TRUE");
    expect(run("=OR(A1>100,B1>100)")).toBe("FALSE");
    expect(run("=ROUND(3.456,2)")).toBe("3.46");
    expect(run("=MOD(7,3)")).toBe("1");
    expect(run("=LEN(C1)")).toBe("3");
    expect(run("=UPPER(C1)")).toBe("ABC");
  });

  it("reports errors with codes", () => {
    expect(run("=A1/0")).toBe("#DIV0");
    expect(run("=NOPE(1)")).toBe("#NAME?");
    expect(run("=1.5.5+1")).toBe("#ERR"); // malformed number
    expect(run("=1 2")).toBe("#ERR"); // trailing tokens
    expect(run('="abc')).toBe("#ERR"); // unterminated string
    expect(run("=AA1")).toBe("#REF"); // beyond A–Z
  });

  it("propagates errors and detects cycles", () => {
    const r = evalSheet({ "0:0": "=1/0", "0:1": "=A1+1", "1:0": "=B2", "1:1": "=A2" });
    expect(r.display["0:1"]).toBe("#DIV0");
    expect(r.display["1:0"]).toBe("#CIRC");
    expect(r.errors["1:1"]).toBe("#CIRC");
  });

  it("treats blank cells as 0 / empty", () => {
    expect(run("=Z99+1")).toBe("1");
    expect(run("=Z99")).toBe("");
  });
});
