import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { applyTheme } from "./prefs";
import "./styles.css";

// Apply the persisted theme before first paint — avoids a dark flash for
// light-theme users. 先应用已存主题再渲染，避免亮色用户看到暗色闪屏。
applyTheme();

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
