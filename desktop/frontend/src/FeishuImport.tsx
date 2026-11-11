// 从飞书导入 panel — paste links → job → live progress.
import { useEffect, useRef, useState } from "react";
import { type FeishuItem, type FeishuJob, feishuApi } from "./api";

const STATUS: Record<string, string> = {
  draft: "未开始",
  running: "导入中",
  paused: "已暂停",
  done: "已完成",
  cancelled: "已取消",
  error: "出错",
};

const BADGE: Record<string, string> = {
  pending: "⌛",
  done: "✅",
  skipped: "↷",
  failed: "✗",
};

export function FeishuImport({
  onDone,
  onOpenDoc,
}: {
  onDone: () => void;
  onOpenDoc: (id: string) => void;
}) {
  const [links, setLinks] = useState("");
  const [job, setJob] = useState<FeishuJob | null>(null);
  const [items, setItems] = useState<FeishuItem[]>([]);
  const [jobs, setJobs] = useState<FeishuJob[]>([]);
  const [err, setErr] = useState("");
  // Refresh the docs list whenever more items have landed, not only at the
  // end — imported docs show up as they finish. 每有新完成项就刷新文档列表。
  const seenDone = useRef(-1);
  const loadJob = async (id: string) => {
    const d = await feishuApi.job(id);
    setJob(d.job);
    setItems(d.items ?? []);
    if (d.job.status === "error") setErr(d.last_error || "导入出错");
    if (d.job.done !== seenDone.current) {
      if (seenDone.current >= 0 || d.job.done > 0) onDone();
      seenDone.current = d.job.done;
    }
  };
  useEffect(() => {
    feishuApi
      .jobs()
      .then((l) => setJobs(l ?? []))
      .catch(() => {});
  }, []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: loadJob is recreated each render; the poll keys on job id/status / 轮询只依赖任务 id 与状态
  useEffect(() => {
    if (!job || job.status !== "running") return;
    const t = setInterval(() => void loadJob(job.id), 2000);
    return () => clearInterval(t);
  }, [job]);
  const start = async () => {
    setErr("");
    const ls = links
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);
    try {
      const { id } = await feishuApi.createJob(ls);
      seenDone.current = -1;
      await loadJob(id);
      feishuApi
        .jobs()
        .then((l) => setJobs(l ?? []))
        .catch(() => {});
    } catch (e) {
      setErr(String(e));
    }
  };
  const act = async (a: "start" | "pause" | "cancel") => {
    if (!job) return;
    try {
      await feishuApi.act(job.id, a);
      await loadJob(job.id);
    } catch (e) {
      setErr(String(e));
    }
  };
  return (
    <div className="feishu-panel">
      {!job && (
        <>
          <textarea
            rows={5}
            placeholder="每行一个飞书链接：/docx/…、/wiki/…、/drive/folder/…"
            value={links}
            onChange={(e) => setLinks(e.target.value)}
          />
          <button type="button" onClick={() => void start()}>
            开始导入
          </button>
        </>
      )}
      {job && (
        <>
          <div className="counts">
            {job.status === "running" && (
              <button type="button" onClick={() => void act("pause")}>
                暂停
              </button>
            )}
            {job.status === "paused" && (
              <button type="button" onClick={() => void act("start")}>
                继续
              </button>
            )}
            {job.status !== "done" && job.status !== "cancelled" && (
              <button type="button" onClick={() => void act("cancel")}>
                取消
              </button>
            )}
            {job.status === "done" && job.failed > 0 && (
              <button type="button" onClick={() => void act("start")}>
                重试失败项
              </button>
            )}
            <b>
              {" "}
              {STATUS[job.status] ?? job.status} — 完成 {job.done} · 跳过 {job.skipped} · 失败{" "}
              {job.failed} · 待处理 {job.pending}
            </b>
            {job.status !== "running" && (
              <button
                type="button"
                className="ghost sm"
                onClick={() => {
                  setJob(null);
                  setItems([]);
                }}
              >
                新的导入
              </button>
            )}
          </div>
          <ul className="fi-items">
            {items.map((it) => (
              <li key={`${it.obj_type}:${it.path}/${it.title}`}>
                {BADGE[it.status] ?? "?"}{" "}
                {it.status === "done" && it.doc_ids?.[0] ? (
                  <button type="button" className="link" onClick={() => onOpenDoc(it.doc_ids[0])}>
                    {it.title}
                  </button>
                ) : (
                  it.title
                )}
                {it.error && <span className="err-sm"> — {it.error}</span>}
              </li>
            ))}
          </ul>
        </>
      )}
      {err && <p className="err">{err}</p>}
      {jobs.length > 0 && (
        <details>
          <summary>历史任务（{jobs.length}）</summary>
          {jobs.map((j) => (
            <button
              type="button"
              key={j.id}
              className="link fi-past"
              onClick={() => {
                seenDone.current = j.done;
                setJob(j);
                void loadJob(j.id);
              }}
            >
              {j.name} · {STATUS[j.status] ?? j.status} · {j.done}/{j.total}
            </button>
          ))}
        </details>
      )}
    </div>
  );
}
