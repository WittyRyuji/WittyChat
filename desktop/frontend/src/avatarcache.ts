// Share in-flight requests and briefly cache failures without making an outage
// permanent. Successful blob URLs stay stable so animated avatars do not reset.
// 合并并发头像请求，失败短时缓存但不永久记住故障；成功的 blob URL 保持动画连续。
export const AVATAR_FAILURE_TTL = 30_000;

export class AvatarCache {
  private entries = new Map<string, { url: string; expires: number }>();
  private pending = new Map<string, Promise<string>>();

  constructor(private failureTTL = AVATAR_FAILURE_TTL) {}

  get(key: string): string | undefined {
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    if (entry.expires <= Date.now()) {
      this.entries.delete(key);
      return undefined;
    }
    return entry.url;
  }

  load(key: string, fetchBlob: () => Promise<Blob>): Promise<string> {
    const cached = this.get(key);
    if (cached !== undefined) return Promise.resolve(cached);
    const existing = this.pending.get(key);
    if (existing) return existing;
    const request = Promise.resolve()
      .then(fetchBlob)
      .then((blob) => {
        const url = URL.createObjectURL(blob);
        this.entries.set(key, { url, expires: Infinity });
        return url;
      })
      .catch(() => {
        this.entries.set(key, { url: "", expires: Date.now() + this.failureTTL });
        return "";
      })
      .finally(() => this.pending.delete(key));
    this.pending.set(key, request);
    return request;
  }
}
