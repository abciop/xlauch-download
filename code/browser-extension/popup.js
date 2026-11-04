/* XDM 下载器助手 —— Popup
 * 与 background.js 消息协议：ping / addTask / exportCookies */
"use strict";

const $ = (id) => document.getElementById(id);

/* ---------- 工具 ---------- */

// HTML 转义，防止资源 URL 注入
function esc(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function send(msg) {
  return new Promise((resolve) => {
    chrome.runtime.sendMessage(msg, (resp) => {
      resolve(resp || { ok: false, error: "无响应" });
    });
  });
}

// 由页面 URL 计算站点标识：去 www. 后取最后两段（如 youtube.com / douyin.com）
function siteOf(pageUrl) {
  try {
    const h = new URL(pageUrl).hostname.toLowerCase().replace(/^www\./, "");
    const parts = h.split(".").filter(Boolean);
    if (parts.length >= 2) return parts.slice(-2).join(".");
    return h;
  } catch (e) {
    return "";
  }
}

function setStatus(el, text, cls) {
  el.textContent = text;
  el.className = "status" + (cls ? " " + cls : "");
}

/* ---------- 连接状态 ---------- */

async function refreshDot() {
  const dot = $("dot");
  const resp = await send({ cmd: "ping" });
  if (resp.ok) {
    dot.textContent = "已连接";
    dot.className = "dot ok";
  } else {
    dot.textContent = "未启动";
    dot.className = "dot no";
  }
}

/* ---------- 当前标签页 ---------- */

async function currentTab() {
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  return tabs && tabs[0] ? tabs[0] : null;
}

/* ---------- 资源列表 ---------- */

function fmtSize(n) {
  if (!n) return "";
  if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(0) + " KB";
  return n + " B";
}

// 展示文件名：新记录取嗅探时提取的 name；旧记录回退到从 URL 推导
function nameOf(it) {
  if (it.name) return it.name;
  try {
    const u = new URL(it.url);
    const q = u.searchParams.get("filename") || u.searchParams.get("fileName");
    if (q) return q;
    let seg = u.pathname.split("/").filter(Boolean).pop() || "";
    try { seg = decodeURIComponent(seg); } catch (e) { /* 保留原文 */ }
    if (/\.[a-z0-9]{1,5}$/i.test(seg)) return seg;
    return u.hostname;
  } catch (e) {
    return it.url;
  }
}

function renderResources(tab) {
  const list = $("res-list");
  chrome.storage.session.get({ xdmRecent: [] }, ({ xdmRecent }) => {
    const items = (xdmRecent || []).filter((x) => tab && x.tabId === tab.id);
    if (!items.length) {
      list.innerHTML = '<div class="empty">暂未嗅探到资源（播放视频后重新打开本弹窗）</div>';
      return;
    }
    list.innerHTML = "";
    for (const it of items.slice(0, 15)) {
      const meta = [it.type || "media", fmtSize(it.size)].filter(Boolean).join(" · ");
      const btn = document.createElement("button");
      btn.className = "res";
      btn.innerHTML =
        '<span class="n">' + esc(nameOf(it)) + "</span>" +
        '<span class="t">' + esc(meta) + "</span>" +
        '<span class="u">' + esc(it.url) + "</span>";
      btn.addEventListener("click", () => {
        send({ cmd: "addTask", url: it.url }).then(() => window.close());
      });
      list.appendChild(btn);
    }
  });
}

/* ---------- 事件绑定 ---------- */

async function init() {
  // 连接状态
  refreshDot();

  // 端口回显
  const { xdmPort } = await chrome.storage.local.get({ xdmPort: 50710 });
  $("port").value = xdmPort || 50710;

  const tab = await currentTab();
  renderResources(tab);

  const pageUrl = tab && tab.url ? tab.url : "";
  const site = siteOf(pageUrl);

  // 下载当前页面
  $("btn-page").addEventListener("click", async () => {
    if (!/^https?:/i.test(pageUrl)) {
      alert("当前页面不是 http/https 地址，无法下载");
      return;
    }
    await send({ cmd: "addTask", url: pageUrl });
    window.close();
  });

  // 导出登录 Cookie
  $("btn-export").addEventListener("click", async () => {
    const st = $("export-status");
    if (!site) {
      setStatus(st, "无法识别当前页面站点", "err");
      return;
    }
    setStatus(st, "导出中…");
    const resp = await send({ cmd: "exportCookies", pageUrl, site });
    if (resp.ok) {
      setStatus(st, "已导出 " + resp.count + " 条 Cookie（" + resp.site + "）", "ok");
    } else {
      setStatus(st, "导出失败：" + (resp.error || "未知错误"), "err");
    }
  });

  // 保存端口
  $("btn-port").addEventListener("click", async () => {
    const v = parseInt($("port").value, 10);
    if (!(v >= 1 && v <= 65535)) {
      alert("端口无效（1-65535）");
      return;
    }
    await chrome.storage.local.set({ xdmPort: v });
    const st = $("export-status");
    setStatus(st, "端口已保存为 " + v + "，重新检测连接…");
    await refreshDot();
    setStatus(st, "端口已保存为 " + v);
  });
}

document.addEventListener("DOMContentLoaded", init);
