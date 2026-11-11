// 从飞书导入 panel — one-click personal import (authorize in the browser,
// the server imports 我的空间 + 我的文档库) or paste links; live progress.
// 一键导入个人文档（浏览器授权后服务端自动导入），或粘贴链接；实时进度。
import { useEffect, useRef, useState } from "react";
import { type FeishuItem, type FeishuJob, feishuApi, openExternal } from "./api";

const PERSONAL = "我的飞书文档";

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
  authorized,
  onDone,
  onOpenDoc,
}: {
  authorized: boolean;
  onDone: () => void;
  onOpenDoc: (id: string) => void;
}) {
  const [waiting, setWaiting] = useState(false);
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
  // After the browser consent, the server creates the personal job; pick it
  // up as soon as it appears. 浏览器授权后服务端建任务，出现即接上进度。
  // biome-ignore lint/correctness/useExhaustiveDependencies: loadJob is recreated each render; polling keys on `waiting` / 轮询只依赖 waiting
  useEffect(() => {
    if (!waiting) return;
    // A new personal job, or the existing one restarted by the consent.
    // 新建的个人任务，或授权后重新启动的原任务。
    const known = new Set(jobs.filter((j) => j.status !== "running").map((j) => j.id));
    const t = setInterval(() => {
      feishuApi
        .jobs()
        .then((l) => {
          const fresh = (l ?? []).find(
            (j) => j.name === PERSONAL && (!known.has(j.id) || j.status === "running")
          );
          if (!fresh) return;
          setWaiting(false);
          setJobs(l ?? []);
          seenDone.current = -1;
          void loadJob(fresh.id);
        })
        .catch(() => {});
    }, 3000);
    const stop = setTimeout(() => setWaiting(false), 10 * 60 * 1000);
    return () => {
      clearInterval(t);
      clearTimeout(stop);
    };
  }, [waiting]);
  const authorize = async () => {
    setErr("");
    try {
      const { url } = await feishuApi.oauthStart();
      await openExternal(url);
      setWaiting(true);
    } catch (e) {
      setErr(String(e));
    }
  };
  const personal = jobs.find((j) => j.name === PERSONAL);
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
        <div className="fi-oneclick">
          {waiting ? (
            <span>请在打开的浏览器页面里同意授权，完成后这里会自动显示导入进度…</span>
          ) : (
            <button type="button" className="primary" onClick={() => void authorize()}>
              {authorized ? "重新授权并导入我的飞书文档" : "一键导入我的飞书文档"}
            </button>
          )}
          {personal && !waiting && (
            <button
              type="button"
              className="link"
              onClick={() => {
                seenDone.current = personal.done;
                setJob(personal);
                void loadJob(personal.id);
              }}
            >
              查看上次导入（{STATUS[personal.status] ?? personal.status} · {personal.done}/
              {personal.total}）
            </button>
          )}
          <span className="err-sm">
            会导入你飞书「我的空间」和「我的文档库」里你自己的文档和表格。
          </span>
        </div>
      )}
      {!job && (
        <details>
          <summary>按链接导入</summary>
          <textarea
            rows={5}
            placeholder="每行一个飞书链接：/docx/…、/wiki/…、/drive/folder/…"
            value={links}
            onChange={(e) => setLinks(e.target.value)}
          />
          <button type="button" onClick={() => void start()}>
            开始导入
          </button>
        </details>
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
