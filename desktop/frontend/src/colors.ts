// Peer color — stable per user id, shared by carets, presence chips and
// sheet/slide focus markers. 对端颜色：按用户 id 哈希取色，同人同色。
const CURSOR_COLORS = [
  "#7c5cfc",
  "#1f9cf0",
  "#00b96b",
  "#f0a41f",
  "#f0524f",
  "#c94fd6",
  "#0fb5ba",
  "#8a6d3b",
];

export function cursorColor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return CURSOR_COLORS[h % CURSOR_COLORS.length];
}
