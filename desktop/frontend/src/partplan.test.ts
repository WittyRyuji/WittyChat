import { describe, expect, it } from "vitest";
import { partPlan } from "./partplan";

describe("partPlan", () => {
  it("splits an exact multiple into whole parts", () => {
    const p = partPlan(24, 8);
    expect(p).toEqual([
      { n: 1, start: 0, end: 8 },
      { n: 2, start: 8, end: 16 },
      { n: 3, start: 16, end: 24 },
    ]);
  });
  it("handles a remainder in the last part", () => {
    const p = partPlan(20, 8);
    expect(p).toHaveLength(3);
    expect(p[2]).toEqual({ n: 3, start: 16, end: 20 });
  });
  it("a size smaller than one part is a single part", () => {
    expect(partPlan(5, 8)).toEqual([{ n: 1, start: 0, end: 5 }]);
  });
  it("offsets are contiguous with no gaps and last end == size", () => {
    for (const [size, part] of [
      [1, 8],
      [100, 7],
      [1024 * 1024 * 17, 8 << 20],
    ] as const) {
      const p = partPlan(size, part);
      expect(p[0].start).toBe(0);
      for (let i = 1; i < p.length; i++) {
        expect(p[i].start).toBe(p[i - 1].end);
        expect(p[i].n).toBe(p[i - 1].n + 1);
      }
      expect(p[p.length - 1].end).toBe(size);
    }
  });
  it("rejects a non-positive partSize", () => {
    expect(() => partPlan(10, 0)).toThrow();
  });
});
