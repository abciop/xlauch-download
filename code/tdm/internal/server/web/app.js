/* Xlauch 下载器前端逻辑：SSE 任务流 / 表格任务列表 / 分类侧栏 / 工具栏 / 浅色暗黑主题 */
"use strict";

const $ = (id) => document.getElementById(id);

let tasks = [];            // 全量任务快照
let curStatus = "all";     // 任务状态过滤
let curCategory = "all";   // 分类过滤（与状态可叠加）
const selected = new Set(); // 选中任务 id
let selFormat = "";        // 视频弹窗选中 format_id
let analyzeURL = "";       // 视频弹窗对应 URL
let toastTimer = null;
let cfgDir = "";           // 当前下载目录（用于"打开下载文件夹"菜单）

/* ---------- 主题 ---------- */

function applyTheme(name) {
  document.documentElement.dataset.theme = name;
  const sw = $("theme-switch");
  if (sw) sw.checked = name === "dark";
  try { localStorage.setItem("tdm-theme", name); } catch (e) { /* 忽略 */ }
}

function initTheme() {
  let t = "light";
  try { t = localStorage.getItem("tdm-theme") || "light"; } catch (e) { /* 忽略 */ }
  applyTheme(t);
}

function toggleTheme() {
  applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");
}

/* ---------- 工具函数 ---------- */

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function fmtBytes(n) {
  if (n == null || n < 0) return "--";
  if (n < 1024) return n.toFixed(0) + " B";
  const u = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n >= 100 ? 0 : 1) + " " + u[i];
}

function fmtSpeed(bps) {
  if (!bps || bps <= 0) return "--";
  return fmtBytes(bps) + "/s";
}

function fmtEta(sec) {
  if (!sec || sec <= 0 || sec > 3600 * 24 * 7) return "--";
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = Math.floor(sec % 60);
  if (h > 0) return h + "时" + m + "分";
  if (m > 0) return m + "分" + s + "秒";
  return s + "秒";
}

function fmtDuration(sec) {
  if (!sec) return "";
  const m = Math.floor(sec / 60), s = Math.floor(sec % 60);
  return m + "分" + (s < 10 ? "0" : "") + s + "秒";
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opt);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || "HTTP " + resp.status);
  return data;
}

/* 前端类型识别（与后端 task.Detect 保持一致逻辑） */
/* 从分享文案中提取真实链接（抖音/快手等 App 复制出来的文本会夹带大量说明和表情） */
function extractURL(raw) {
  const s = (raw || "").trim();
  const magnet = /magnet:\?[^\s"'<>]+/i.exec(s);
  if (magnet) return magnet[0];
  const http = /https?:\/\/[^\s"'<>`]+/i.exec(s);
  if (http) return http[0].replace(/[）】」，。；、！？]+$/, "");
  return s;
}

function detectKind(raw) {
  const u = extractURL(raw).toLowerCase();
  if (u.startsWith("magnet:?")) return "bt";
  if (u.endsWith(".torrent")) return "bt";
  if (u.includes(".m3u8") || u.includes("m3u8?")) return "hls";
  const sites = ["youtube.com", "youtu.be", "twitter.com", "x.com", "reddit.com", "redd.it",
    "tiktok.com", "douyin.com", "vimeo.com", "bilibili.com", "b23.tv", "facebook.com",
    "fb.watch", "instagram.com", "twitch.tv", "dailymotion.com", "youku.com", "iqiyi.com",
    "v.qq.com", "weibo.com", "xiaohongshu.com", "kuaishou.com", "ixigua.com",
    "soundcloud.com", "bandcamp.com", "nicovideo.jp", "pinterest.com", "t.me", "telegram.me"];
  for (const s of sites) if (u.includes(s)) return "video";
  return "http";
}

/* 按扩展名分类（侧栏"分类"组） */
const CAT_EXT = {
  video: ["mp4", "mkv", "avi", "mov", "flv", "webm", "ts", "m4v", "wmv", "mpg", "mpeg", "3gp"],
  music: ["mp3", "flac", "wav", "aac", "m4a", "ogg", "ape", "wma", "opus"],
  app: ["exe", "msi", "apk", "dmg", "bin", "jar"],
  archive: ["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso", "cab"],
  doc: ["pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "csv", "md", "epub"],
  image: ["png", "jpg", "jpeg", "gif", "webp", "bmp", "svg", "ico", "tif", "tiff", "avif", "heic", "raw"],
};

function catOf(t) {
  /* 依次尝试文件名、标题、URL：文件名可能被重命名去掉扩展名，需回退到 URL 判断 */
  for (const raw of [t.filename, t.title, t.url]) {
    if (!raw) continue;
    const name = String(raw).split(/[?#]/)[0];
    const base = name.slice(name.lastIndexOf("/") + 1);
    if (!base.includes(".")) continue;
    const ext = base.slice(base.lastIndexOf(".") + 1).toLowerCase();
    for (const [cat, exts] of Object.entries(CAT_EXT)) {
      if (exts.includes(ext)) return cat;
    }
  }
  return "other";
}

function matchesFilter(t) {
  if (curCategory !== "all" && catOf(t) !== curCategory) return false;
  switch (curStatus) {
    case "all": return true;
    case "downloading": return t.status === "running" || t.status === "queued";
    default: return t.status === curStatus; // paused/done/error
  }
}

/* ---------- 渲染 ---------- */

const KIND_ICON = { http: "⬇", hls: "▶", bt: "🧲", video: "🎬" };
const STATUS_TAG = {
  queued: ["排队中", "queue"], running: ["下载中", "run"], paused: ["已暂停", "pause"],
  done: ["已完成", "done"], error: ["失败", "err"],
};

function render() {
  const visible = tasks.filter((t) => matchesFilter(t));

  /* 统计与计数 */
  let speed = 0;
  const counts = { all: 0, downloading: 0, paused: 0, done: 0, error: 0,
    video: 0, music: 0, app: 0, archive: 0, doc: 0, image: 0, other: 0 };
  for (const t of tasks) {
    counts.all++;
    counts[catOf(t)]++;
    if (t.status === "running" || t.status === "queued") counts.downloading++;
    else if (t.status === "paused") counts.paused++;
    else if (t.status === "done") counts.done++;
    else if (t.status === "error") counts.error++;
    if (t.status === "running") speed += t.speed || 0;
  }
  for (const [k, v] of Object.entries(counts)) {
    const el = $("c-" + k);
    if (el) el.textContent = v;
  }
  $("sb-count").textContent = tasks.length + " 个任务";
  $("sb-speed").textContent = "总速度: " + fmtBytes(speed) + "/s";

  /* 空状态 */
  $("empty-state").style.display = tasks.length === 0 || visible.length === 0 ? "" : "none";

  /* 表格行 */
  let html = "";
  for (const t of visible) {
    const [tagTxt, tagCls] = STATUS_TAG[t.status] || [t.status, "queue"];
    const pct = Math.max(0, Math.min(100, t.progress || 0));
    const mfillCls = t.status === "done" ? "done" : t.status === "error" ? "err" : "";
    const hasTitle = !!(t.title || t.filename);
    const name = hasTitle ? (t.title || t.filename) : t.url;
    const isSel = selected.has(t.id);

    let sizeCell;
    if (t.total > 0) sizeCell = fmtBytes(t.done) + " / " + fmtBytes(t.total);
    else if (t.done > 0) sizeCell = fmtBytes(t.done);
    else sizeCell = t.status === "done" ? (t.filePath ? "" : "--") : "--";

    let etaCell = "--";
    if (t.status === "running" && t.eta > 0) etaCell = fmtEta(t.eta);
    else if (t.status === "done") etaCell = "";
    else if (t.status === "error" && t.error) etaCell = '<span class="err-text" title="' + esc(t.error) + '">' + esc(t.error.slice(0, 12)) + "</span>";

    let speedCell = t.status === "running" ? fmtSpeed(t.speed) : "";

    /* 行内操作 */
    const ops = [];
    const op = (act, icon, tip, cls) =>
      '<button class="op ' + (cls || "") + '" data-act="' + act + '" data-id="' + t.id + '" title="' + tip + '">' + icon + "</button>";
    if (t.status === "running" || t.status === "queued") ops.push(op("pause", "⏸", "暂停"));
    if (t.status === "paused") ops.push(op("resume", "▶", "继续"));
    if (t.status === "error") ops.push(op("retry", "↻", "重试"));
    if (t.status === "done" || t.filePath) ops.push(op("openDir", "📂", "打开所在目录"));
    ops.push(op("remove", "✕", "删除任务", "del"));

    const errAttr = t.error ? ' title="' + esc(t.error) + '"' : "";
    html += '<tr class="' + (isSel ? "sel" : "") + '" data-id="' + t.id + '" data-url="' + esc(t.url) + '"' + errAttr + ">" +
      '<td class="col-check"><input type="checkbox" class="rowcheck" data-id="' + t.id + '"' + (isSel ? " checked" : "") + "></td>" +
      '<td class="col-name"><div class="tname"><span class="kico ' + t.kind + '">' + (KIND_ICON[t.kind] || "⬇") + "</span>" +
        '<span class="tt' + (hasTitle ? "" : " no-title") + '" title="' + esc(name) + '">' + esc(name) + "</span></div></td>" +
      "<td>" + sizeCell + "</td>" +
      '<td class="col-prog"><div class="mprog"><div class="mbar"><div class="mfill ' + mfillCls + '" style="width:' + pct + '%"></div></div>' +
        '<span class="mpct">' + pct.toFixed(1) + "%</span></div></td>" +
      "<td>" + speedCell + "</td>" +
      '<td><span class="tag ' + tagCls + '">' + tagTxt + "</span></td>" +
      "<td>" + etaCell + "</td>" +
      '<td class="col-op"><div class="ops">' + ops.join("") + "</div></td>" +
      "</tr>";
  }
  $("tbody").innerHTML = html;

  /* 全选框同步（仅对可见行） */
  const visChecks = visible.length > 0;
  const allSel = visChecks && visible.every((t) => selected.has(t.id));
  $("check-all").checked = allSel;
  $("check-all").indeterminate = visChecks && !allSel && visible.some((t) => selected.has(t.id));
}

/* ---------- SSE ---------- */

function connectSSE() {
  const es = new EventSource("/api/events");
  es.addEventListener("tasks", (e) => {
    tasks = JSON.parse(e.data);
    // 清理已消失任务的选中态
    const ids = new Set(tasks.map((t) => t.id));
    for (const id of selected) if (!ids.has(id)) selected.delete(id);
    render();
  });
  es.addEventListener("clipboard", (e) => {
    const { text } = JSON.parse(e.data);
    showClipboardToast(text);
  });
  es.onerror = () => { /* EventSource 自动重连 */ };
}

/* ---------- 剪贴板 toast ---------- */

function showClipboardToast(text) {
  $("toast-url").textContent = text;
  $("toast").classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => $("toast").classList.remove("show"), 8000);
}

/* ---------- 添加任务 ---------- */

async function addTask(raw, extra = {}) {
  const url = extractURL(raw);
  const kind = detectKind(url);
  if (kind === "video") {
    await openVideoModal(url, extra.dir || "");
    return;
  }
  await api("POST", "/api/tasks", { url, dir: extra.dir || "", kind: "" });
}

async function quickAdd() {
  const input = $("url-input");
  const url = input.value.trim();
  if (!url) { input.focus(); return; }
  try {
    await addTask(url);
    input.value = "";
  } catch (err) {
    alert("添加失败：" + err.message);
  }
}

/* ---------- 拖拽添加（种子文件 / 链接文本） ---------- */

/* 上传单个 .torrent 种子文件 */
async function addTorrentFile(file) {
  const bytes = new Uint8Array(await file.arrayBuffer());
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  await api("POST", "/api/torrent", { data: btoa(bin), filename: file.name, dir: "" });
}

/* 任务菜单「打开BT种子…」：弹出文件选择器，选中后与拖拽走同一上传通道 */
function openTorrentPicker() {
  const input = document.createElement("input");
  input.type = "file";
  input.accept = ".torrent";
  input.onchange = async () => {
    const f = input.files && input.files[0];
    if (!f) return;
    try {
      await addTorrentFile(f);
    } catch (err) {
      alert(`「${f.name}」添加失败：` + err.message);
    }
  };
  input.click();
}

/* ---------- 浏览器扩展插件 ---------- */

let extPreparedPath = "";

/* 第 1 步：解压内嵌扩展到本机数据目录 */
async function prepareExtension() {
  const btn = $("ext-prepare");
  btn.disabled = true;
  btn.textContent = "解压中...";
  try {
    const res = await api("POST", "/api/extension/prepare", {});
    extPreparedPath = res.path;
    $("ext-path").textContent = res.path + "（" + res.count + " 个文件，选择此目录加载）";
    $("ext-opendir").style.display = "";
    btn.textContent = "重新解压";
  } catch (err) {
    alert("解压失败：" + err.message);
    btn.textContent = "解压扩展到本机";
  }
  btn.disabled = false;
}

/* 第 2 步：一键拉起浏览器扩展管理页 */
async function openBrowserExtPage(browser) {
  try {
    await api("POST", "/api/extension/open", { browser });
  } catch (err) {
    alert(err.message);
  }
}

/* 按钮下拉菜单：复用右键菜单样式，在锚点按钮下方弹出
 * items: [id, 图标, 文案, 点击回调] */
function showBtnMenu(anchor, items) {
  let m = $("btn-menu");
  if (!m) {
    m = document.createElement("div");
    m.id = "btn-menu";
    m.className = "ctxmenu";
    document.body.appendChild(m);
  }
  const cleanup = () => {
    m.classList.remove("show");
    m.removeEventListener("click", onPick);
    document.removeEventListener("mousedown", onAway);
    window.removeEventListener("resize", cleanup);
  };
  const onPick = (e) => {
    const item = e.target.closest(".ctxitem");
    if (!item) return;
    const hit = items.find((x) => x[0] === item.dataset.bm);
    cleanup();
    if (hit && hit[3]) hit[3]();
  };
  const onAway = (e) => { if (!e.target.closest("#btn-menu")) cleanup(); };
  m.innerHTML = items.map(([id, icon, label]) =>
    '<div class="ctxitem" data-bm="' + id + '"><span>' + icon + '</span>' + label + '</div>').join("");
  const r = anchor.getBoundingClientRect();
  m.style.right = "auto";
  m.style.left = Math.max(8, Math.min(r.left, window.innerWidth - 180)) + "px";
  m.style.top = (r.bottom + 5) + "px";
  m.classList.add("show");
  m.addEventListener("click", onPick);
  document.addEventListener("mousedown", onAway);
  window.addEventListener("resize", cleanup);
}

/* 全局接管拖拽：阻止 WebView2 把拖入文件变成"浏览器下载" */
function bindDragDrop() {
  const hint = $("drop-hint");
  let depth = 0;
  const hasFiles = (e) => {
    const types = e.dataTransfer ? e.dataTransfer.types : null;
    return !!types && Array.from(types).includes("Files");
  };

  window.addEventListener("dragenter", (e) => {
    e.preventDefault();
    if (!hasFiles(e)) return;
    depth++;
    hint.classList.add("show");
  });
  window.addEventListener("dragover", (e) => {
    e.preventDefault(); // 关键：不拦截就会走浏览器默认的文件下载
    if (hasFiles(e)) hint.classList.add("show");
  });
  window.addEventListener("dragleave", (e) => {
    if (!hasFiles(e)) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0) hint.classList.remove("show");
  });
  window.addEventListener("drop", async (e) => {
    e.preventDefault();
    depth = 0;
    hint.classList.remove("show");
    const files = Array.from((e.dataTransfer && e.dataTransfer.files) || []);
    if (files.length > 0) {
      for (const f of files) {
        if (f.name.toLowerCase().endsWith(".torrent")) {
          try { await addTorrentFile(f); }
          catch (err) { alert(`「${f.name}」添加失败：` + err.message); }
        } else {
          alert(`「${f.name}」不是种子文件，仅支持拖入 .torrent 文件或链接文本`);
        }
      }
      return;
    }
    // 拖入的是文本（如链接）
    const text = e.dataTransfer ? (e.dataTransfer.getData("text/plain") || e.dataTransfer.getData("text")) : "";
    if (text && text.trim()) {
      try { await addTask(text.trim()); }
      catch (err) { alert("添加失败：" + err.message); }
    }
  });
}

/* ---------- 视频画质弹窗 ---------- */

function resHeight(res) {
  const m = /(\d{3,4})\s*x\s*(\d{3,4})/i.exec(res || "");
  return m ? parseInt(m[2], 10) : (res === "best" ? 9999 : 0);
}

async function openVideoModal(url, dir) {
  analyzeURL = url;
  selFormat = "";
  $("mask-video").classList.add("show");
  $("v-title").textContent = "分析中...";
  $("v-sub").textContent = "";
  $("v-site").textContent = "";
  $("v-thumb").style.visibility = "hidden";
  const isDouyin = /douyin\.com|iesdouyin\.com/i.test(url);
  $("v-formats").innerHTML = '<div style="color:var(--text2);padding:20px;text-align:center;">' +
    (isDouyin ? "正在启动内置浏览器解析抖音视频（可能需要数秒）..." : "正在调用 yt-dlp 解析，首次使用会自动下载组件...") +
    "</div>";
  $("v-dir").value = dir;
  $("v-download").disabled = true;

  try {
    const res = await api("POST", "/api/analyze", { url });
    $("v-title").textContent = res.title || url;
    $("v-sub").textContent = [res.site, fmtDuration(res.duration)].filter(Boolean).join(" · ");
    $("v-site").textContent = res.site ? "· " + res.site : "";
    if (res.thumbnail) {
      const img = $("v-thumb");
      img.src = res.thumbnail;
      img.style.visibility = "visible";
    }
    const fmts = [...(res.formats || [])].sort((a, b) => resHeight(b.resolution) - resHeight(a.resolution));
    let html = "";
    fmts.forEach((f) => {
      const is4K = /2160|4k/i.test(f.resolution || "");
      const sub = [f.fps ? f.fps + "fps" : "", f.filesize > 0 ? fmtBytes(f.filesize) : (f.tbr > 0 ? Math.round(f.tbr) + "k" : "")].filter(Boolean).join(" · ");
      html += '<div class="fmt" data-fid="' + esc(f.formatId) + '">' +
        '<b class="' + (is4K ? "r4k" : "") + '">' + esc(f.resolution) + "</b>" +
        "<span>" + esc(sub) + "</span></div>";
    });
    $("v-formats").innerHTML = html;
    if (fmts.length > 0) selectFormat(fmts[0].formatId);
    $("v-download").disabled = false;
  } catch (err) {
    $("v-formats").innerHTML = '<div style="color:var(--err);padding:16px;text-align:center;line-height:1.7;">' + esc(err.message) + "</div>";
  }
}

function selectFormat(fid) {
  selFormat = fid;
  document.querySelectorAll("#v-formats .fmt").forEach((el) => {
    el.classList.toggle("sel", el.dataset.fid === fid);
  });
}

async function downloadVideo() {
  if (!analyzeURL) return;
  try {
    await api("POST", "/api/tasks", {
      url: analyzeURL, dir: $("v-dir").value.trim(), kind: "video", quality: selFormat,
    });
    $("mask-video").classList.remove("show");
  } catch (err) {
    alert("添加失败：" + err.message);
  }
}

/* ---------- 批量 m3u8 ---------- */

async function batchGo() {
  const lines = $("batch-input").value.split("\n").map((s) => s.trim()).filter(Boolean);
  if (lines.length === 0) return;
  const dir = $("batch-dir").value.trim();
  const btn = $("batch-go");
  btn.disabled = true;
  btn.textContent = "添加中...";
  let ok = 0, fail = 0;
  for (const line of lines) {
    const url = extractURL(line);
    try {
      await api("POST", "/api/tasks", { url, dir, kind: "" });
      ok++;
    } catch { fail++; }
  }
  btn.disabled = false;
  btn.textContent = "全部添加";
  if (fail === 0) $("mask-batch").classList.remove("show");
  else alert("已添加 " + ok + " 个，失败 " + fail + " 个");
  $("batch-input").value = "";
}

/* ---------- 任务操作 ---------- */

async function doAction(id, action, delFiles = false) {
  try {
    await api("POST", "/api/tasks/" + id + "/action", { action, delFiles });
  } catch (err) {
    alert("操作失败：" + err.message);
  }
}

/* 选中任务的批量操作 */
function selectedIds() { return [...selected]; }

/* 删除确认弹窗：返回 "files"（强制删除含文件）/ "record"（仅移除任务）/ "cancel" */
let deleteResolve = null;

function askDelete(ids) {
  if (deleteResolve) closeDelete("cancel");
  return new Promise((resolve) => {
    deleteResolve = resolve;
    const t = ids.length === 1 ? tasks.find((x) => x.id === ids[0]) : null;
    const name = t ? (t.title || t.filename || t.url) : "";
    $("del-msg").textContent = ids.length === 1
      ? "确定要删除任务「" + name + "」吗？"
      : "已选中 " + ids.length + " 个任务，请选择删除方式：";
    $("mask-delete").classList.add("show");
  });
}

function closeDelete(result) {
  $("mask-delete").classList.remove("show");
  const fn = deleteResolve;
  deleteResolve = null;
  if (fn) fn(result);
}

/* ---------- 任务右键菜单 ---------- */

function hideCtxMenu() { $("ctx-menu").classList.remove("show"); }

function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).catch(() => copyFallback(text));
  } else {
    copyFallback(text);
  }
}

function copyFallback(text) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0;";
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); } catch (e) { /* 忽略 */ }
  ta.remove();
}

function showCtxMenu(x, y) {
  const sel = tasks.filter((t) => selected.has(t.id));
  if (sel.length === 0) return;
  const items = [];
  if (sel.some((t) => t.status === "running" || t.status === "queued")) items.push(["pause", "⏸", "暂停"]);
  if (sel.some((t) => t.status === "paused")) items.push(["resume", "▶", "继续"]);
  if (sel.some((t) => t.status === "error")) items.push(["retry", "↻", "重试"]);
  if (sel.every((t) => t.status === "done" || t.filePath)) items.push(["openDir", "📂", "打开所在目录"]);
  if (sel.length === 1) items.push(["copy", "🔗", "复制下载链接"]);
  items.push(["remove", "✕", "删除任务", "danger"]);

  const el = $("ctx-menu");
  el.innerHTML = items.map(([act, icon, label, cls]) =>
    '<div class="ctxitem' + (cls ? " " + cls : "") + '" data-act="' + act + '">' +
    "<span>" + icon + "</span>" + label + "</div>").join("");
  el.classList.add("show");
  const r = el.getBoundingClientRect();
  el.style.left = Math.max(8, Math.min(x, window.innerWidth - r.width - 8)) + "px";
  el.style.top = Math.max(8, Math.min(y, window.innerHeight - r.height - 8)) + "px";
}

async function ctxAction(act) {
  const ids = [...selected];
  hideCtxMenu();
  if (ids.length === 0) return;
  if (act === "copy") {
    const t = tasks.find((x) => selected.has(x.id));
    if (t) copyText(t.url);
    return;
  }
  if (act === "remove") {
    const choice = await askDelete(ids);
    if (choice === "cancel") return;
    for (const id of ids) await doAction(id, "remove", choice === "files");
    selected.clear();
    render();
    return;
  }
  for (const id of ids) await doAction(id, act);
}

async function bulkAction(action, delFiles = false) {
  const ids = selectedIds();
  if (ids.length === 0) { alert("请先勾选任务行"); return; }
  for (const id of ids) await doAction(id, action, delFiles);
  selected.clear();
}

async function pauseAll() {
  for (const t of tasks) {
    if (t.status === "running" || t.status === "queued") await doAction(t.id, "pause");
  }
}

/* 全部开始：恢复暂停的任务，并重试失败的任务 */
async function startAll() {
  for (const t of tasks) {
    if (t.status === "paused") await doAction(t.id, "resume");
    else if (t.status === "error") await doAction(t.id, "retry");
  }
}

/* 删除选中任务（沿用三选项确认弹窗） */
async function removeSelected() {
  const ids = selectedIds();
  if (ids.length === 0) { alert("请先勾选任务行"); return; }
  const choice = await askDelete(ids);
  if (choice === "cancel") return;
  for (const id of ids) await doAction(id, "remove", choice === "files");
  selected.clear();
  render();
}

/* 清空所有任务 */
async function clearAllTasks() {
  const ids = tasks.map((t) => t.id);
  if (ids.length === 0) { alert("当前没有任务"); return; }
  const choice = await askDelete(ids);
  if (choice === "cancel") return;
  for (const id of ids) await doAction(id, "remove", choice === "files");
  selected.clear();
  render();
}

/* 手动刷新任务列表 */
async function refreshTasks() {
  try {
    const data = await api("GET", "/api/tasks");
    tasks = data.tasks || [];
    render();
  } catch (err) {
    alert("刷新失败：" + err.message);
  }
}

/* 打开设置中的下载文件夹 */
function openDownloadFolder() {
  if (!cfgDir) { alert("尚未设置下载目录"); return; }
  api("POST", "/api/open", { path: cfgDir }).catch((err) => alert("打开失败：" + err.message));
}

/* 从剪贴板读取并添加下载 */
async function addFromClipboard() {
  let text = "";
  try {
    text = (await navigator.clipboard.readText()).trim();
  } catch (err) {
    alert("读取剪贴板失败：" + err.message);
    return;
  }
  if (!text || text.length > 4096) { alert("剪贴板中没有可用的下载链接"); return; }
  try {
    await addTask(text);
  } catch (err) {
    alert("添加失败：" + err.message);
  }
}

/* ---------- 菜单栏 ---------- */

function closeMenus() {
  document.querySelectorAll("#menubar .menu").forEach((m) => m.classList.remove("open"));
}

async function menuCommand(cmd) {
  switch (cmd) {
    case "add": $("url-input").focus(); break;
    case "openTorrent": openTorrentPicker(); break;
    case "startAll": await startAll(); break;
    case "pauseAll": await pauseAll(); break;
    case "quit":
      if (confirm("确定要退出 Xlauch 下载器吗？")) {
        api("POST", "/api/quit", {}).catch((err) => alert("退出失败：" + err.message));
      }
      break;
    case "openDir": openDownloadFolder(); break;
    case "extension": $("mask-ext").classList.add("show"); break;
    case "clipboard": await addFromClipboard(); break;
    case "resume": await bulkAction("resume"); break;
    case "pause": await bulkAction("pause"); break;
    case "remove": await removeSelected(); break;
    case "refresh": await refreshTasks(); break;
    case "clear": await clearAllTasks(); break;
    case "engines": await openTools(); break;
    case "settings": await openSettings(); break;
    case "theme": toggleTheme(); break;
    case "website":
      api("POST", "/api/open-url", { url: "https://xdm.xlauch.com" })
        .catch((err) => alert("打开官网失败：" + err.message));
      break;
  }
}

function bindMenubar() {
  document.querySelectorAll("#menubar .mtitle").forEach((btn) => {
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      const menu = btn.parentElement;
      const wasOpen = menu.classList.contains("open");
      closeMenus();
      if (!wasOpen) menu.classList.add("open");
    });
  });
  document.querySelectorAll("#menubar .menu").forEach((menu) => {
    menu.addEventListener("mouseenter", () => {
      if (document.querySelector("#menubar .menu.open")) {
        closeMenus();
        menu.classList.add("open");
      }
    });
  });
  document.querySelectorAll("#menubar .mitem").forEach((item) => {
    item.addEventListener("click", () => {
      closeMenus();
      menuCommand(item.dataset.cmd);
    });
  });
  document.addEventListener("mousedown", (e) => {
    if (!e.target.closest("#menubar")) closeMenus();
  });
  window.addEventListener("blur", closeMenus);
}

/* ---------- 设置 ---------- */

function fillConfig(cfg) {
  cfgDir = cfg.downloadDir || "";
  $("cfg-dir").value = cfg.downloadDir || "";
  $("cfg-threads").value = cfg.maxThreads;
  $("cfg-hls").value = cfg.hlsConcurrency;
  $("cfg-active").value = cfg.maxActive;
  $("cfg-port").value = cfg.port || 50710;
  $("cfg-peers").value = cfg.btMaxPeers;
  $("cfg-proxy").value = cfg.proxy || "";
  $("cfg-clip").checked = !!cfg.checkClipboard;
  $("cfg-mp4").checked = !!cfg.preferMp4;
  $("cfg-close").value = cfg.closeAction === "exit" ? "exit" : "tray";
  $("cfg-cookies-browser").value = cfg.cookiesFromBrowser || "";
  $("cfg-lang").value = cfg.language || "zh-CN";
  updateDirHint(cfg.downloadDir);
  refreshCookieStatus();
}

function updateDirHint(dir) {
  $("sb-dir").textContent = dir ? "保存到 " + dir : "";
}

async function openSettings() {
  fillRuntime();
  try {
    fillConfig(await api("GET", "/api/config"));
  } catch (err) {
    alert("读取配置失败：" + err.message);
  }
  $("mask-settings").classList.add("show");
}

/* 运行信息：端口与地址取自当前页面来源 */
function fillRuntime() {
  const u = location.origin;
  $("cfg-addr").textContent = u;
  $("cfg-addr").href = u;
}

async function saveConfig() {
  const body = {
    downloadDir: $("cfg-dir").value.trim(),
    maxThreads: parseInt($("cfg-threads").value, 10) || 16,
    hlsConcurrency: parseInt($("cfg-hls").value, 10) || 8,
    maxActive: parseInt($("cfg-active").value, 10) || 3,
    port: parseInt($("cfg-port").value, 10) || 50710,
    btMaxPeers: parseInt($("cfg-peers").value, 10) || 40,
    proxy: $("cfg-proxy").value.trim(),
    checkClipboard: $("cfg-clip").checked,
    preferMp4: $("cfg-mp4").checked,
    closeAction: $("cfg-close").value,
    cookiesFromBrowser: $("cfg-cookies-browser").value,
    language: $("cfg-lang").value || "zh-CN",
  };
  try {
    const cfg = await api("POST", "/api/config", body);
    updateDirHint(cfg.downloadDir);
    $("mask-settings").classList.remove("show");
  } catch (err) {
    alert("保存失败：" + err.message);
  }
}

/* ---------- 视频 Cookie 管理 ---------- */

async function refreshCookieStatus() {
  try {
    const st = await api("GET", "/api/cookies");
    const stat = $("cfg-cookies-stat");
    if (st.cookiesFile && st.cookiesFileValid) {
      stat.textContent = st.cookiesFile;
      stat.className = "cstat ok";
      stat.title = st.cookiesFile;
    } else if (st.cookiesFile) {
      stat.textContent = "文件不存在: " + st.cookiesFile;
      stat.className = "cstat bad";
      stat.title = st.cookiesFile;
    } else {
      stat.textContent = "未导入";
      stat.className = "cstat";
      stat.title = "";
    }
    const box = $("cfg-cookie-sites");
    const sites = st.sites || [];
    if (sites.length === 0) {
      box.innerHTML = '<span class="cnone">暂无</span>';
    } else {
      box.innerHTML = sites.map((s) =>
        '<span class="csite" title="' + esc(s.site + " · " + s.count + " 条 · " + s.updatedAt) + '">' +
        esc(s.site) + ' <i class="cnt">' + s.count + '</i><b class="cx" data-site="' + esc(s.site) + '" title="删除">✕</b></span>').join("");
    }
  } catch { /* 忽略 */ }
}

async function importCookiesFile() {
  const btn = $("cfg-cookies-import");
  btn.disabled = true;
  btn.textContent = "选择中...";
  try {
    const res = await api("POST", "/api/pick-file", { initial: "" });
    if (res.path) {
      await api("POST", "/api/config", { cookiesFile: res.path });
      await refreshCookieStatus();
    }
  } catch (err) {
    alert("导入失败：" + err.message);
  }
  btn.disabled = false;
  btn.textContent = "导入 cookies.txt...";
}

async function clearCookiesFile() {
  try {
    await api("POST", "/api/config", { cookiesFile: "" });
    await refreshCookieStatus();
  } catch (err) {
    alert("清除失败：" + err.message);
  }
}

async function resetConfig() {
  if (!confirm("确定恢复所有设置为默认值吗？")) return;
  try {
    fillConfig(await api("POST", "/api/config/reset", {}));
  } catch (err) {
    alert("恢复默认失败：" + err.message);
  }
}

/* ---------- 工具管理 ---------- */

function fillTools(st) {
  const yOk = st.ytdlpPresent, fOk = st.ffmpegPresent;
  $("t-ytdlp-st").textContent = yOk ? "已安装" : "未安装";
  $("t-ytdlp-st").className = yOk ? "ok" : "no";
  $("t-ytdlp-ver").textContent = yOk
    ? "版本 " + (st.ytdlpVersion || "?") + " — 支持 1800+ 站点视频解析（最高 4K）"
    : "支持 1800+ 站点视频解析（YouTube/Bilibili/TikTok 等，最高 4K），首次使用需下载（约 17MB）";
  $("t-ffmpeg-st").textContent = fOk ? "已安装" : "未安装";
  $("t-ffmpeg-st").className = fOk ? "ok" : "no";
  $("t-ffmpeg-ver").textContent = fOk
    ? (st.ffmpegVersion || "ffmpeg") + " — 视频流合并与转封装就绪"
    : "视频流合并与转封装（m3u8 → MP4、音视频合并），按需下载（约 180MB）";
  $("t-ytdlp-btn").disabled = yOk;
  $("t-ffmpeg-btn").disabled = fOk;
}

async function openTools() {
  $("mask-tools").classList.add("show");
  try { fillTools(await api("GET", "/api/tools")); } catch { /* 忽略 */ }
}

async function installTool(which) {
  const btn = $(which === "ytdlp" ? "t-ytdlp-btn" : "t-ffmpeg-btn");
  const old = btn.textContent;
  btn.disabled = true;
  btn.innerHTML = '<span class="spin"></span> 下载中...';
  try {
    fillTools(await api("POST", "/api/tools/" + which));
  } catch (err) {
    alert("下载失败：" + err.message);
    fillTools(await api("GET", "/api/tools").catch(() => ({})));
  }
  btn.innerHTML = old;
}

/* ---------- 事件绑定与启动 ---------- */

function bind() {
  /* 禁用 F5 刷新页面；Esc 关闭菜单与右键菜单 */
  window.addEventListener("keydown", (e) => {
    if (e.key === "F5") { e.preventDefault(); return; }
    if (e.key === "Escape") { closeMenus(); hideCtxMenu(); }
  });

  /* 菜单栏 */
  bindMenubar();

  /* 侧栏主题开关 */
  $("theme-switch").addEventListener("change", (e) => {
    applyTheme(e.target.checked ? "dark" : "light");
  });

  /* 拖拽添加 */
  bindDragDrop();

  /* 添加栏：下拉选择「新建下载 / 打开BT种子」 */
  $("btn-add").addEventListener("click", () => {
    showBtnMenu($("btn-add"), [
      ["new", "＋", "新建下载（输入框 URL）", () => quickAdd()],
      ["bt", "🌱", "打开BT种子…", openTorrentPicker],
    ]);
  });
  $("url-input").addEventListener("keydown", (e) => { if (e.key === "Enter") quickAdd(); });

  /* 工具栏 */
  $("tb-add").addEventListener("click", () => {
    showBtnMenu($("tb-add"), [
      ["url", "🔗", "添加 URL（聚焦输入框）", () => { $("url-input").focus(); }],
      ["bt", "🌱", "打开BT种子…", openTorrentPicker],
    ]);
  });
  $("tb-batch").addEventListener("click", () => $("mask-batch").classList.add("show"));
  $("tb-resume").addEventListener("click", () => bulkAction("resume"));
  $("tb-pause").addEventListener("click", () => bulkAction("pause"));
  $("tb-opendir").addEventListener("click", () => bulkAction("openDir"));
  $("tb-pauseall").addEventListener("click", pauseAll);
  $("tb-remove").addEventListener("click", removeSelected);

  /* 侧栏过滤：任务状态与分类可叠加 */
  document.querySelectorAll(".sitem[data-s]").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".sitem[data-s]").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      curStatus = btn.dataset.s;
      render();
    });
  });
  document.querySelectorAll(".sitem[data-c]").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".sitem[data-c]").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      curCategory = btn.dataset.c;
      render();
    });
  });

  /* 全选 */
  $("check-all").addEventListener("change", (e) => {
    const on = e.target.checked;
    tasks.filter((t) => matchesFilter(t)).forEach((t) => {
      if (on) selected.add(t.id); else selected.delete(t.id);
    });
    render();
  });

  /* 表格：勾选 / 行操作 / 双击打开目录（事件委托） */
  $("tbody").addEventListener("change", (e) => {
    const chk = e.target.closest(".rowcheck");
    if (!chk) return;
    if (chk.checked) selected.add(chk.dataset.id); else selected.delete(chk.dataset.id);
    render();
  });
  $("tbody").addEventListener("click", (e) => {
    const opBtn = e.target.closest(".op");
    if (opBtn) {
      const { act, id } = opBtn.dataset;
      if (act === "remove") {
        askDelete([id]).then((choice) => {
          if (choice === "cancel") return;
          doAction(id, "remove", choice === "files");
        });
      } else {
        doAction(id, act);
      }
      return;
    }
    const chk = e.target.closest(".rowcheck");
    if (chk) return;
    const tr = e.target.closest("tr");
    if (tr) {
      selected.clear();
      selected.add(tr.dataset.id);
      render();
    }
  });
  $("tbody").addEventListener("dblclick", (e) => {
    const tr = e.target.closest("tr");
    if (tr) doAction(tr.dataset.id, "openDir");
  });

  /* 任务区右键菜单 */
  $("tbody").addEventListener("contextmenu", (e) => {
    const tr = e.target.closest("tr");
    if (!tr || !tr.dataset.id) { hideCtxMenu(); return; }
    e.preventDefault();
    if (!selected.has(tr.dataset.id)) {
      selected.clear();
      selected.add(tr.dataset.id);
      render();
    }
    showCtxMenu(e.clientX, e.clientY);
  });
  $("ctx-menu").addEventListener("click", (e) => {
    const item = e.target.closest(".ctxitem");
    if (item) ctxAction(item.dataset.act);
  });
  document.addEventListener("mousedown", (e) => {
    if (!e.target.closest("#ctx-menu")) hideCtxMenu();
  });
  document.querySelector(".tablewrap").addEventListener("scroll", hideCtxMenu);
  window.addEventListener("resize", hideCtxMenu);

  /* 删除确认弹窗 */
  $("del-cancel").addEventListener("click", () => closeDelete("cancel"));
  $("del-record").addEventListener("click", () => closeDelete("record"));
  $("del-file").addEventListener("click", () => closeDelete("files"));
  $("mask-delete").addEventListener("mousedown", (e) => {
    if (e.target === e.currentTarget) closeDelete("cancel");
  });

  /* 弹窗内部 */
  $("batch-go").addEventListener("click", batchGo);
  $("cfg-save").addEventListener("click", saveConfig);
  $("cfg-open-dir").addEventListener("click", () => {
    const dir = $("cfg-dir").value.trim();
    if (dir) api("POST", "/api/open", { path: dir }).catch((err) => alert("打开失败：" + err.message));
  });
  $("cfg-browse").addEventListener("click", async () => {
    const btn = $("cfg-browse");
    btn.disabled = true;
    btn.textContent = "选择中...";
    try {
      const res = await api("POST", "/api/pick-dir", { initial: $("cfg-dir").value.trim() });
      if (res.path) $("cfg-dir").value = res.path;
    } catch (err) {
      alert("选择目录失败：" + err.message);
    }
    btn.disabled = false;
    btn.textContent = "浏览";
  });
  $("cfg-open-browser").addEventListener("click", () => {
    api("POST", "/api/open-browser", {}).catch((err) => alert("打开失败：" + err.message));
  });
  $("cfg-reset").addEventListener("click", resetConfig);
  $("cfg-cancel").addEventListener("click", () => $("mask-settings").classList.remove("show"));
  $("cfg-cookies-import").addEventListener("click", importCookiesFile);
  $("cfg-cookies-clear").addEventListener("click", clearCookiesFile);
  $("cfg-cookie-sites").addEventListener("click", async (e) => {
    const x = e.target.closest(".cx");
    if (!x) return;
    try {
      await api("POST", "/api/cookies/clear", { site: x.dataset.site });
      await refreshCookieStatus();
    } catch (err) {
      alert("删除失败：" + err.message);
    }
  });
  $("cfg-addr").addEventListener("click", (e) => {
    e.preventDefault();
    api("POST", "/api/open-browser", {}).catch((err) => alert("打开失败：" + err.message));
  });
  $("t-ytdlp-btn").addEventListener("click", () => installTool("ytdlp"));
  $("t-ffmpeg-btn").addEventListener("click", () => installTool("ffmpeg"));
  $("v-download").addEventListener("click", downloadVideo);
  $("v-formats").addEventListener("click", (e) => {
    const el = e.target.closest(".fmt");
    if (el) selectFormat(el.dataset.fid);
  });

  /* 剪贴板 toast */
  $("toast-yes").addEventListener("click", () => {
    const text = $("toast-url").textContent;
    $("toast").classList.remove("show");
    addTask(text).catch((err) => alert("添加失败：" + err.message));
  });
  $("toast-no").addEventListener("click", () => {
    $("toast").classList.remove("show");
    clearTimeout(toastTimer);
  });

  /* 模态框关闭：[data-close] 与点击遮罩 */
  document.querySelectorAll("[data-close]").forEach((btn) => {
    btn.addEventListener("click", () => btn.closest(".mask").classList.remove("show"));
  });
  document.querySelectorAll(".mask").forEach((mask) => {
    mask.addEventListener("mousedown", (e) => { if (e.target === mask) mask.classList.remove("show"); });
  });

  /* 浏览器扩展插件弹窗 */
  $("ext-prepare").addEventListener("click", prepareExtension);
  $("ext-opendir").addEventListener("click", () => {
    if (!extPreparedPath) return;
    api("POST", "/api/open", { path: extPreparedPath }).catch((err) => alert("打开失败：" + err.message));
  });
  $("ext-chrome").addEventListener("click", () => openBrowserExtPage("chrome"));
  $("ext-edge").addEventListener("click", () => openBrowserExtPage("edge"));
}

async function init() {
  initTheme();
  bind();
  connectSSE();
  try {
    const data = await api("GET", "/api/tasks");
    tasks = data.tasks || [];
    render();
  } catch { /* SSE 初始快照会补上 */ }
  api("GET", "/api/config").then((cfg) => {
    fillConfig(cfg);
  }).catch(() => {});
}

init();
