/* XDM 下载器助手 —— 页面内悬浮下载标识（content script）
 * 鼠标悬停在视频 / 音频 / 大图 / 媒体链接上片刻，右上角弹出「下载」浮条，点击直接推送 XDM。
 * 全部 UI 放在 Shadow DOM，避免被页面样式污染；容器 pointer-events:none，不影响页面交互。 */
"use strict";

(() => {
  const HOVER_DELAY = 350;   // 悬停多久才弹出（毫秒），避免鼠标划过就闪
  const LEAVE_DELAY = 180;   // 移出后延迟收起，留出移到浮条上点击的时间
  const MIN_IMG = 120;       // 图片最小边长（px），过滤 1px 追踪图与站点小图标
  const MAX_UP = 6;          // 向上查找目标的最大层数
  const MEDIA_EXT = /\.(mp4|m4v|mkv|webm|avi|mov|flv|ts|m3u8|mpd|mp3|m4a|aac|flac|wav|ogg|zip|rar|7z|tar|gz|pdf|exe|msi|iso|apk|dmg|torrent)(?:[?#]|$)/i;

  // 只在 http/https 页面生效；跳过 XDM 自身的 Web UI 页面
  if (location.protocol !== "http:" && location.protocol !== "https:") return;
  if ((location.hostname === "127.0.0.1" || location.hostname === "localhost") &&
      document.querySelector("nav#menubar")) return;

  const ignored = new Set();   // 本页被 ✕ 忽略的资源
  let host = null, bar = null, txt = null;
  let curEl = null, curUrl = "", pendingEl = null;
  let showTimer = 0, hideTimer = 0;

  /* ---------- 目标识别 ---------- */

  function isHttp(s) {
    return !!s && /^https?:\/\//i.test(s) && !ignored.has(s);
  }

  function urlOf(el) {
    const tag = el.tagName ? el.tagName.toLowerCase() : "";
    if (tag === "video" || tag === "audio") {
      // blob: 地址由播放器动态生成，无法直接下载，跳过
      for (const s of [el.currentSrc, el.src]) if (isHttp(s)) return s;
      const so = el.querySelector("source");
      if (so && isHttp(so.src)) return so.src;
      return "";
    }
    if (tag === "img") {
      const s = el.currentSrc || el.src;
      if (!isHttp(s)) return "";
      const r = el.getBoundingClientRect();
      if (Math.min(r.width, r.height) < MIN_IMG) return "";
      return s;
    }
    if (tag === "a") {
      const h = el.href || "";
      if (isHttp(h) && (MEDIA_EXT.test(h) || el.hasAttribute("download"))) return h;
    }
    return "";
  }

  function pickTarget(node) {
    let el = node instanceof Element ? node : (node && node.parentElement);
    for (let i = 0; el && i < MAX_UP; i++) {
      const u = urlOf(el);
      if (u) return { el, url: u };
      el = el.parentElement;
    }
    return null;
  }

  /* ---------- 浮条 UI ---------- */

  function buildUI() {
    if (host) return;
    host = document.createElement("div");
    host.setAttribute("data-xdm-hover", "");
    host.style.cssText =
      "position:fixed;left:0;top:0;width:0;height:0;z-index:2147483647;pointer-events:none;";
    const sr = host.attachShadow({ mode: "open" });
    sr.innerHTML = `
      <style>
        .bar {
          position: absolute; display: none; align-items: center; gap: 8px;
          padding: 5px 7px 5px 5px; border-radius: 999px;
          background: rgba(24, 28, 36, .92); color: #fff;
          box-shadow: 0 6px 20px rgba(0,0,0,.35), inset 0 0 0 1px rgba(255,255,255,.08);
          font: 600 13px/1 "Segoe UI", -apple-system, sans-serif;
          pointer-events: auto; user-select: none; white-space: nowrap;
        }
        .bar.show { display: flex; }
        .brand {
          width: 26px; height: 26px; border-radius: 8px; flex: 0 0 auto;
          background: linear-gradient(135deg, #3b82f6, #2563eb);
          display: flex; align-items: center; justify-content: center;
        }
        .brand svg { width: 15px; height: 15px; }
        .dl {
          display: flex; align-items: center; gap: 6px;
          padding: 4px 5px; border-radius: 7px; cursor: pointer;
        }
        .dl:hover { background: rgba(255,255,255,.13); }
        .dl svg { width: 15px; height: 15px; flex: 0 0 auto; }
        .close {
          width: 22px; height: 22px; padding: 0; border: 0; border-radius: 50%;
          margin-left: 1px; flex: 0 0 auto; cursor: pointer;
          background: rgba(255,255,255,.13); color: rgba(255,255,255,.72);
          font: 11px/1 "Segoe UI", sans-serif;
          display: flex; align-items: center; justify-content: center;
        }
        .close:hover { background: rgba(255,255,255,.26); color: #fff; }
      </style>
      <div class="bar">
        <div class="brand">
          <svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2.2" stroke-linecap="round">
            <path d="M12 4v10m0 0l-3.6-3.6m3.6 3.6l3.6-3.6M5 19h14"/>
          </svg>
        </div>
        <div class="dl">
          <svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round">
            <path d="M12 4v10m0 0l-3.6-3.6m3.6 3.6l3.6-3.6M5 19h14"/>
          </svg>
          <span class="t">下载</span>
        </div>
        <button class="close" title="本次不再提示">✕</button>
      </div>`;
    document.documentElement.appendChild(host);

    bar = sr.querySelector(".bar");
    txt = sr.querySelector(".t");
    bar.addEventListener("pointerenter", () => clearTimeout(hideTimer));
    bar.addEventListener("pointerleave", () => scheduleHide());
    sr.querySelector(".dl").addEventListener("click", onDownload);
    sr.querySelector(".close").addEventListener("click", onClose);
  }

  /* 定位到目标元素右上角内侧；放不下时翻到元素上方或贴边 */
  function place(el) {
    const r = el.getBoundingClientRect();
    // 阈值宁小勿大：媒体链接常是单行文字（高度约 19px），过严会导致浮条不出现
    if (r.width < 30 || r.height < 14) return false;
    if (r.bottom < 0 || r.top > innerHeight || r.right < 0 || r.left > innerWidth) return false;
    bar.classList.add("show");
    const bw = bar.offsetWidth || 130, bh = bar.offsetHeight || 36;
    let top = r.top + 10;
    if (top + bh > innerHeight - 8) top = Math.max(8, r.top - bh - 10);
    let left = r.right - bw - 10;
    left = Math.max(8, Math.min(left, innerWidth - bw - 8));
    bar.style.left = Math.round(left) + "px";
    bar.style.top = Math.round(top) + "px";
    return true;
  }

  /* ---------- 交互 ---------- */

  function scheduleShow(hit) {
    clearTimeout(showTimer);
    pendingEl = hit.el;
    const el = hit.el, url = hit.url;
    showTimer = setTimeout(() => {
      pendingEl = null;
      curEl = el;
      curUrl = url;
      buildUI();
      txt.textContent = "下载";
      if (!place(el)) scheduleHide(0);
    }, HOVER_DELAY);
  }

  function scheduleHide(delay = LEAVE_DELAY) {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      curEl = null;
      pendingEl = null;
      if (bar) bar.classList.remove("show");
    }, delay);
  }

  function onOver(e) {
    if (host && pathOf(e).includes(host)) return;  // 鼠标在浮条上
    const hit = pickTarget(e.target);
    if (!hit) { scheduleHide(); return; }
    clearTimeout(hideTimer);
    if (hit.el === curEl || hit.el === pendingEl) return;
    scheduleShow(hit);
  }

  function onOut(e) {
    if (!curEl && !pendingEl) return;
    if (host && pathOf(e).includes(host)) return;
    const to = e.relatedTarget;
    if (to && ((curEl && curEl.contains(to)) || (host && to === host))) return;
    scheduleHide();
  }

  function pathOf(e) {
    return e && typeof e.composedPath === "function" ? e.composedPath() : [];
  }

  async function onDownload(e) {
    e.preventDefault();
    e.stopPropagation();
    const url = curUrl;
    if (!url) return;
    txt.textContent = "添加中…";
    try {
      const resp = await chrome.runtime.sendMessage({ cmd: "addTask", url });
      txt.textContent = (resp && resp.ok) ? "已添加" : "失败，请检查 XDM";
    } catch (err) {
      txt.textContent = "失败，请检查 XDM";
    }
    setTimeout(() => { scheduleHide(0); }, 1400);
  }

  function onClose(e) {
    e.preventDefault();
    e.stopPropagation();
    if (curUrl) ignored.add(curUrl);
    scheduleHide(0);
  }

  /* ---------- 事件绑定 ---------- */

  document.addEventListener("pointerover", onOver, true);
  document.addEventListener("pointerout", onOut, true);
  addEventListener("scroll", () => { if (curEl) scheduleHide(0); }, true);
  addEventListener("resize", () => { if (curEl) scheduleHide(0); }, true);
})();
