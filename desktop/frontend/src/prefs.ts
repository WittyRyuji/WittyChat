// Server-backed user preferences with a synchronous localStorage mirror.
// Server is source of truth (cross-device); mirror keeps reads sync for the
// rest of the UI. On first load, server values overwrite the mirror and any
// legacy localStorage-only keys are migrated up once.
//
// 服务端用户偏好 + localStorage 镜像。服务端为准（跨设备同步），镜像让 UI
// 保持同步读取。首次加载时服务端值覆盖镜像，仅存于本地的旧键一次性迁移上行。

import { api } from "./api";

const PREF_RE = /^(pref\.|wittychat\.mail\.)/;

export async function loadPrefs(): Promise<void> {
  try {
    const res = await api.getPrefs();
    const server = res.prefs ?? {};
    const legacy: Record<string, unknown> = {};
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (!k || !PREF_RE.test(k) || k in server) continue;
      try {
        legacy[k] = JSON.parse(localStorage.getItem(k) ?? "null");
      } catch {
        legacy[k] = localStorage.getItem(k);
      }
    }
    for (const [k, v] of Object.entries(server)) {
      localStorage.setItem(k, JSON.stringify(v));
    }
    if (Object.keys(legacy).length > 0) {
      await api.mergePrefs(legacy).catch(() => {});
    }
  } catch {
    // offline / server older than prefs API — mirror still works locally
  }
}

export function prefGet<T>(key: string, fallback: T): T {
  const raw = localStorage.getItem(key);
  if (raw == null) return fallback;
  try {
    return JSON.parse(raw) as T;
  } catch {
    return raw as unknown as T;
  }
}

export function prefSet(key: string, value: unknown): void {
  localStorage.setItem(key, JSON.stringify(value));
  void api.mergePrefs({ [key]: value }).catch(() => {});
}
