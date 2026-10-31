import { Browser } from "@wailsio/runtime";
import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { inWails } from "./api";
import { applyTheme } from "./prefs";
import "./styles.css";

// Apply the persisted theme before first paint — avoids a dark flash for
// light-theme users. 先应用已存主题再渲染，避免亮色用户看到暗色闪屏。
applyTheme();

// In the Wails shell, route every absolute http(s) link to the system
// default browser — otherwise clicks navigate the WebView itself (or
// spawn a bare WebView2 window) and kill the session. Internal shell URLs
// (wails.localhost, relative) are left alone.
// 壳内把所有 http(s) 外链交给系统默认浏览器打开；否则点击会原地导航
// WebView（或弹出裸 WebView2 窗口）毁掉会话。壳内/相对链接不动。
if (inWails) {
  document.addEventListener("click", (e) => {
    const a = (e.target as HTMLElement).closest?.("a[href]") as HTMLAnchorElement | null;
    const href = a?.href ?? "";
    if (!a || !/^https?:\/\//i.test(href)) return;
    if (new URL(href).origin === location.origin) return;
    e.preventDefault();
    Browser.OpenURL(href).catch(() => {});
  });
}

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
