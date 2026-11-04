/* XDM 下载器助手 —— Service Worker
 * 功能：右键菜单发送下载 / 嗅探页面媒体资源 / 通知反馈 */
"use strict";

const DEFAULT_PORT = 50710;
const API_KEY = "xdmPort";

async function apiBase() {
  const { xdmPort } = await chrome.storage.local.get(API_KEY);
  return "http://127.0.0.1:" + (xdmPort || DEFAULT_PORT);
}

/* ---------- 通知 ---------- */

function notify(title, message) {
  try {
    chrome.notifications.create({
      type: "basic",
      iconUrl: chrome.runtime.getURL("icon.png"),
      title,
      message: String(message || "").slice(0, 180),
    });
  } catch (e) { /* 通知失败不影响主流程 */ }
}

/* ---------- 添加下载任务 ---------- */

async function addTask(url) {
  try {
    const resp = await fetch((await apiBase()) + "/api/tasks", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url }),
    });
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}));
      throw new Error(data.error || "HTTP " + resp.status);
    }
    notify("已添加到 XDM 下载器", url);
    return true;
  } catch (err) {
    notify("XDM 添加失败", (err && err.message) || "无法连接 XDM（请确认程序已启动）");
    return false;
  }
}

/* ---------- 右键菜单 ---------- */

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({ id: "xdm-link", title: "使用 XDM 下载此链接", contexts: ["link"] });
    chrome.contextMenus.create({ id: "xdm-image", title: "使用 XDM 下载此图片", contexts: ["image"] });
    chrome.contextMenus.create({ id: "xdm-media", title: "使用 XDM 下载此媒体", contexts: ["video", "audio"] });
    chrome.contextMenus.create({ id: "xdm-selection", title: "使用 XDM 下载选中文本中的链接", contexts: ["selection"] });
    chrome.contextMenus.create({ id: "xdm-page", title: "使用 XDM 下载当前页面", contexts: ["page"] });
  });
});

chrome.contextMenus.onClicked.addListener((info) => {
  let url = "";
  switch (info.menuItemId) {
    case "xdm-link": url = info.linkUrl; break;
    case "xdm-image": url = info.srcUrl; break;
    case "xdm-media": url = info.srcUrl || info.linkUrl; break;
    case "xdm-selection": {
      const m = /https?:\/\/[^\s"'<>]+/i.exec(info.selectionText || "");
      url = m ? m[0] : "";
      break;
    }
    case "xdm-page": url = info.pageUrl; break;
  }
  if (url) addTask(url);
  else notify("XDM 下载器助手", "没有识别到可下载的链接");
});

/* ---------- 媒体资源嗅探 ---------- */

const MAX_RECENT = 30;
const SKIP_TYPE = /text\/|image\/|javascript|json|css|font\/|manifest/i;
const MEDIA_TYPE = /video\/|audio\/|mpegurl|mp2t|octet-stream|dash\+xml/i;

// 从响应头/URL 提取文件名：Content-Disposition > filename 查询参数 > 带扩展名的 URL 末段
function extractName(details) {
  for (const h of details.responseHeaders || []) {
    if ((h.name || "").toLowerCase() !== "content-disposition") continue;
    const v = h.value || "";
    let m = /filename\*=(?:utf-8'')?([^;]+)/i.exec(v); // RFC 5987
    if (m) { try { return decodeURIComponent(m[1].replace(/["']/g, "").trim()); } catch (e) { /* 编码异常时回退 */ } }
    m = /filename="?([^";]+)"?/i.exec(v);
    if (m) {
      let s = m[1].trim();
      if (/%[0-9a-f]{2}/i.test(s)) { try { s = decodeURIComponent(s); } catch (e) { /* 保留原文 */ } }
      return s;
    }
  }
  try {
    const u = new URL(details.url);
    // 抖音等 CDN 常把文件名放在 filename 查询参数里（searchParams 已自动解码）
    const q = u.searchParams.get("filename") || u.searchParams.get("fileName");
    if (q) return q;
    let seg = u.pathname.split("/").filter(Boolean).pop() || "";
    try { seg = decodeURIComponent(seg); } catch (e) { /* 保留原文 */ }
    if (/\.[a-z0-9]{1,5}$/i.test(seg)) return seg; // 末段必须像文件名，避免把长哈希目录当文件名
  } catch (e) { /* URL 解析失败 */ }
  return "";
}

chrome.webRequest.onHeadersReceived.addListener((details) => {
  try {
    if (!details.url || !/^https?:/i.test(details.url)) return;
    if (details.tabId < 0) return; // 扩展自身/后台请求
    let ct = "", cl = 0;
    for (const h of details.responseHeaders || []) {
      const n = (h.name || "").toLowerCase();
      if (n === "content-type") ct = h.value || "";
      else if (n === "content-length") cl = parseInt(h.value, 10) || 0;
    }
    const isMedia = MEDIA_TYPE.test(ct);
    const bigFile = cl >= 2 * 1024 * 1024 && !SKIP_TYPE.test(ct);
    if (!isMedia && !bigFile) return;
    const rec = {
      tabId: details.tabId,
      name: extractName(details),
      url: details.url,
      type: (ct.split(";")[0] || "").trim(),
      size: cl,
      ts: Date.now(),
    };
    chrome.storage.session.get({ xdmRecent: [] }, ({ xdmRecent }) => {
      const list = xdmRecent || [];
      if (list.some((x) => x.url === rec.url)) return;
      list.unshift(rec);
      chrome.storage.session.set({ xdmRecent: list.slice(0, MAX_RECENT) });
    });
  } catch (e) { /* 忽略单条嗅探异常 */ }
}, { urls: ["<all_urls>"] }, ["responseHeaders"]);

/* ---------- 消息处理（popup 调用） ---------- */

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  (async () => {
    try {
      if (msg.cmd === "addTask") {
        const ok = await addTask(msg.url);
        sendResponse({ ok });
      } else if (msg.cmd === "ping") {
        const resp = await fetch((await apiBase()) + "/api/tasks");
        sendResponse({ ok: resp.ok });
      } else if (msg.cmd === "exportCookies") {
        // 导出当前标签页站点的全部 Cookie（含 httpOnly）到 XDM
        const all = await chrome.cookies.getAll({ url: msg.pageUrl });
        if (!all.length) throw new Error("未读取到该站点的 Cookie（可能未登录或浏览器限制读取）");
        // 跳过空名 Cookie：RFC 6265 允许无名 Cookie（如 "=delete" 清除标记），
        // 抖音等站点的 CDN 会下发，属无效数据，曾导致服务端整批拒绝
        const payload = all
          .filter((c) => (c.name || "").trim())
          .map((c) => ({
            name: c.name,
            value: c.value,
            domain: c.domain,
            path: c.path,
            expires: c.expirationDate || 0,
            httpOnly: !!c.httpOnly,
            secure: !!c.secure,
          }));
        if (!payload.length) throw new Error("该站点的 Cookie 全部为空名条目，无有效数据");
        const resp = await fetch((await apiBase()) + "/api/cookies/import", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ site: msg.site, cookies: payload }),
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || "HTTP " + resp.status);
        sendResponse({ ok: true, count: data.count, site: data.site });
      } else {
        sendResponse({ ok: false, error: "未知命令" });
      }
    } catch (err) {
      sendResponse({ ok: false, error: (err && err.message) || String(err) });
    }
  })();
  return true; // 异步响应
});
