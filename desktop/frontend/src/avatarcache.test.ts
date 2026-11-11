import { afterEach, describe, expect, it, vi } from "vitest";
import { AvatarCache } from "./avatarcache";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("avatar requests", () => {
  it("shares simultaneous requests and their stable blob URL", async () => {
    const cache = new AvatarCache();
    let resolve: (blob: Blob) => void = () => {};
    const fetchBlob = vi.fn(() => new Promise<Blob>((r) => (resolve = r)));
    const makeURL = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:shared");
    const requests = Array.from({ length: 50 }, () => cache.load("user:v1", fetchBlob));
    await Promise.resolve();
    expect(fetchBlob).toHaveBeenCalledTimes(1);
    resolve(new Blob(["avatar"]));
    expect(await Promise.all(requests)).toEqual(Array(50).fill("blob:shared"));
    expect(makeURL).toHaveBeenCalledTimes(1);
    expect(await cache.load("user:v1", fetchBlob)).toBe("blob:shared");
    expect(fetchBlob).toHaveBeenCalledTimes(1);
  });

  it("backs off failures, then retries without poisoning the version", async () => {
    vi.useFakeTimers();
    const cache = new AvatarCache();
    const fetchBlob = vi.fn<() => Promise<Blob>>().mockRejectedValue(new Error("404"));
    expect(await cache.load("group:v1", fetchBlob)).toBe("");
    for (let i = 0; i < 50; i++) await cache.load("group:v1", fetchBlob);
    expect(fetchBlob).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(30_001);
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:recovered");
    fetchBlob.mockResolvedValue(new Blob(["recovered"]));
    expect(await cache.load("group:v1", fetchBlob)).toBe("blob:recovered");
    expect(fetchBlob).toHaveBeenCalledTimes(2);
  });

  it("does not share a failed version or identity with a different key", async () => {
    const cache = new AvatarCache();
    const failed = vi.fn(() => Promise.reject(new Error("503")));
    const success = vi.fn(() => Promise.resolve(new Blob(["new"])));
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:new");
    await cache.load("session1:user:v1", failed);
    expect(await cache.load("session1:user:v2", success)).toBe("blob:new");
    expect(await cache.load("session2:user:v1", success)).toBe("blob:new");
    expect(success).toHaveBeenCalledTimes(2);
  });
});
