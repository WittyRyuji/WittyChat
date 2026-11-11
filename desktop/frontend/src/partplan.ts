// partPlan splits a size into 1-based parts of partSize bytes —
// {n, start, end} with start inclusive / end exclusive, contiguous and
// covering [0, size). Pure so it's unit-testable in node.
// 分片计划：按 partSize 把 size 切成 1 起编号的分片 {n,start,end}——
// start 含 / end 不含，连续无缝覆盖 [0,size)。纯函数便于单测。
export type PartSpan = { n: number; start: number; end: number };

export function partPlan(size: number, partSize: number): PartSpan[] {
  if (partSize <= 0) throw new Error("partSize must be positive");
  if (size <= 0) return [{ n: 1, start: 0, end: 0 }];
  const count = Math.ceil(size / partSize);
  const out: PartSpan[] = [];
  for (let i = 0; i < count; i++) {
    out.push({ n: i + 1, start: i * partSize, end: Math.min(size, (i + 1) * partSize) });
  }
  return out;
}
