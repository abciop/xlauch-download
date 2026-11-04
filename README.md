# Xlauch 下载器（XDM）

> 极速多协议下载工具：多线程 HTTP · m3u8/HLS · 磁力 BT（DHT）· 1800+ 站点视频解析 · 抖音专用解析 · 浏览器扩展接管。
>
> Windows 单文件绿色版，界面基于系统 WebView2 渲染，完全免费、无广告、不收集数据。

- 官网：<https://xdm.xlauch.com>
- 官网源码：[`code/website/index.html`](code/website/index.html)（浏览器直接打开即可预览）

---

## 一、功能特性

| 能力 | 说明 |
| --- | --- |
| 多线程 HTTP 加速 | 动态分片并发，断点续传，中断重开任务不作废 |
| m3u8 / HLS 流媒体 | 分片高并发下载后自动合并，配置 ffmpeg 后自动转封装为 MP4 |
| 磁力链接 & BT | 内置 DHT 分布式网络，无 tracker 也能发现节点；支持磁力与 `.torrent`（菜单/拖拽打开），逐分片 SHA1 校验 |
| 1800+ 视频站点 | 集成 yt-dlp 内核，YouTube / Bilibili / TikTok 等自动列出全部画质（最高 4K） |
| 抖音专用解析 | 内置无头 WebView2 完成平台签名校验，公开作品免登录解析；导入登录 Cookie 后可下登录可见内容 |
| 剪贴板监听 | 复制链接即弹出下载提示，自动从分享文案中提取真实链接 |
| 浏览器扩展 | 网页视频/音频资源自动嗅探（显示文件名与大小）、悬停「下载」浮条、右键推送链接、一键导出登录 Cookie |
| Cookie 登录态 | 三级来源自动择优：扩展推送 > `cookies.txt` > 读取本机浏览器（Chrome / Edge / Firefox） |
| 任务管理 | 按状态 + 类型双维度过滤，批量暂停/恢复/删除，删除时可选保留文件 |
| 深色模式 & 托盘 | 暗黑主题一键切换，关闭窗口收进托盘后台继续下载 |

---

## 二、技术栈

- **语言**：Go（`module tdm`，`go 1.22`；仓库内置工具链为 go1.27.3 windows/amd64）
- **UI**：原生 WebView2 窗口（`github.com/jchv/go-webview2`），失败自动回退默认浏览器；前端为纯 HTML/CSS/JS，由 `//go:embed` 内嵌进 exe
- **服务**：本机回环 HTTP API，默认 `127.0.0.1:50710`（端口被占用时自动顺延）
- **产物**：单文件 `dist\XDM.exe`，无第三方运行时依赖（WebView2 由 Windows 10/11 自带或自动回退）
- **浏览器扩展**：原生 JS（Manifest V3），无需编译

### 目录结构

```
xlauch-download/
├── build.bat / build.ps1        # 一键构建、测试、运行、清理（根级入口）
├── dist/                        # 构建产物（已 gitignore）
│   ├── XDM.exe
│   └── xdm-browser-extension.zip
├── tools/                       # 本地 Go 工具链（已 gitignore，不入库）
│   ├── go/bin/go.exe
│   └── go-env.ps1               # 设置 GOPATH/PATH 的辅助脚本
└── code/
    ├── tdm/                     # Go 主程序
    │   ├── main.go              # 入口：配置 → 引擎 → 服务 → 原生窗口
    │   ├── go.mod / go.sum
    │   ├── winres/、rsrc_windows_amd64.syso、app.ico   # 图标与版本资源
    │   ├── cmd/dhttest/         # DHT get_peers 诊断工具
    │   └── internal/
    │       ├── config/          # 配置与数据目录
    │       ├── task/            # 任务模型、队列、持久化
    │       ├── engine/          # 多引擎路由
    │       ├── httpdl/          # 多线程 HTTP 引擎
    │       ├── hls/             # m3u8/HLS 引擎
    │       ├── bt/              # BT：bencode / DHT / tracker / peer 协议
    │       ├── ytdlp/           # yt-dlp 站点视频引擎
    │       ├── douyin/          # 抖音 WebView2 解析引擎
    │       ├── server/          # HTTP API + 内嵌 Web UI + 扩展解压
    │       │   ├── web/         # 前端 index.html / app.js / style.css
    │       │   └── webext/      # 扩展内嵌目录（构建时从 code/browser-extension 同步）
    │       └── ui/              # WebView2 窗口、托盘、剪贴板、文件对话框
    ├── browser-extension/       # 浏览器扩展源码（MV3）
    └── website/                 # 官网静态页面（index.html / style.css / app.js）
```

---

## 三、开发环境

| 项 | 要求 |
| --- | --- |
| 操作系统 | Windows 10 / 11（64 位） |
| Go | 1.22 及以上；仓库构建脚本默认使用 `tools\go\bin\go.exe` |
| PowerShell | Windows PowerShell 5.1 或更高（构建脚本为 `.ps1`） |
| WebView2 | 建议安装 Evergreen Runtime；缺失时程序自动回退到默认浏览器（更稳妥的做法见下） |
| 可选 | `ffmpeg`（m3u8 转 MP4）、`yt-dlp`（视频解析）——首次使用会按需下载到数据目录 |

> **关于 `tools/`**：该目录已被 `.gitignore` 排除，克隆仓库后不存在 Go 工具链。请二选一：
> 1. 自行下载 Go SDK 解压到 `tools\go\`（使 `tools\go\bin\go.exe` 存在）；或
> 2. 修改 `build.ps1` 顶部的 `$go` 变量指向系统已安装的 `go.exe`。

---

## 四、构建与打包

### 常用命令（在仓库根目录执行）

```bat
build.bat            :: 构建 + 签名 → dist\XDM.exe（同时同步/打包浏览器扩展）
build.bat run        :: 构建 + 签名 + 启动程序
build.bat test       :: go test ./...
build.bat clean      :: 删除 dist\XDM.exe
build.bat help       :: 查看用法
```

等价写法：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\build.ps1
```

### 构建流程（`build.ps1` 内部步骤）

1. **校验工具链**：确认 `tools\go\bin\go.exe` 存在，否则直接报错退出。
2. **准备扩展**（`Invoke-ExtensionPackage`）：
   - 将 `code\browser-extension\` 全量同步到 `code\tdm\internal\server\webext\`（供 `//go:embed all:webext` 编译期内嵌）；
   - 删除旧包并重新打包 `dist\xdm-browser-extension.zip`（`manifest.json` 位于压缩包根目录）。
3. **编译**：在 `code\tdm` 下执行

   ```powershell
   go build -trimpath -ldflags="-s -w -H windowsgui" -o ..\..\dist\XDM.exe .
   ```

   `-H windowsgui` 表示不弹控制台窗口。
4. **代码签名**（`Invoke-Sign`）：找不到受信任证书时自动创建自签名证书 `CN=Xlauch`，并导入当前用户的「受信任根 / 受信任发布者」，随后用 SHA-256 做 Authenticode 签名并 `Unblock-File`。

### 构建产物

| 文件 | 说明 |
| --- | --- |
| `dist\XDM.exe` | 主程序，约 8.7 MB，单文件绿色版 |
| `dist\xdm-browser-extension.zip` | 浏览器扩展分发包（约 9 KB） |

> 编译或签名失败最常见的原因是 **`XDM.exe` 正在运行**（文件被占用）。请先从托盘退出程序再构建。
> 官网下载区的 SHA-256 校验值需在每次发版后同步更新。

### 手动编译（不使用脚本）

```powershell
. .\tools\go-env.ps1        # 可选：把 tools\go\bin 加入 PATH
cd code\tdm
go build -trimpath -ldflags="-s -w -H windowsgui" -o ..\..\dist\XDM.exe .
```

### 测试

```powershell
build.bat test              # go test ./...
```

- `internal/bt/bencode_test.go`：bencode 编解码单元测试，离线可跑。
- `internal/bt/e2e_test.go`：磁力端到端测试，需要外网与可用做种，耗时较长，CI 中建议按 `-run TestMagnetE2E` 显式触发。

---

## 五、浏览器扩展

源码位于 `code\browser-extension\`，Manifest V3，无需编译：

```
manifest.json      # 权限、content_scripts、background service worker、右键菜单
background.js      # 资源嗅探记录、addTask 推送、Cookie 导出、连接 50710 端口
content.js         # 页面内悬停 350ms 显示「下载」浮条（Shadow DOM 隔离）
popup.html/js      # 弹窗：资源列表（文件名 + 类型/大小）、导出 Cookie、连接设置
icon.png
```

- 构建时由 `build.ps1` 自动同步到 `code\tdm\internal\server\webext\` 并打包 zip，**不要手工编辑 `webext/`**（会在下次构建被覆盖）。
- 修改扩展后必须重新执行 `build.bat`，否则内嵌版本与 zip 不会更新。
- 扩展默认连接 `http://127.0.0.1:50710`；若在 XDM 设置里改了端口，需在扩展弹窗「连接设置」中同步修改并保存。

---

## 六、配置与数据目录

数据目录（`DataDir()`）规则：

1. exe 同目录下存在 `portable.flag` → 便携模式，数据写在 exe 同目录；
2. 否则为 `%LOCALAPPDATA%\TDM`；
3. 非 Windows 环境为 `~/.tdm`。

关键文件：

```
<DataDir>/
├── config.json        # 用户配置（设置页保存时写入）
├── bin/               # 按需下载的 yt-dlp / ffmpeg
├── browser-extension/ # 「帮助 → 插件下载与使用」一键解压的扩展目录
└── *.tdm.btstate      # BT 任务断点状态（随任务文件同目录生成，完成后删除）
```

`config.json` 主要字段及默认值：

| 字段 | 默认值 | 含义 |
| --- | --- | --- |
| `downloadDir` | `d:\xdm`（无 D 盘则回退「下载」目录） | 默认下载目录 |
| `maxThreads` | 16 | HTTP 分片并发数 |
| `hlsConcurrency` | 8 | m3u8 分片并发数 |
| `btMaxPeers` | 40 | BT 单任务最大连接数 |
| `maxActive` | 3 | 同时运行任务数 |
| `port` | 50710 | 本机 API 端口（被占用自动顺延） |
| `proxy` | 空 | `http://` 或 `socks5://` 代理 |
| `checkClipboard` | true | 剪贴板监听 |
| `preferMp4` | true | m3u8 完成后有 ffmpeg 则转封装 MP4 |
| `closeAction` | `tray` | 关闭窗口：收起托盘 / 直接退出 |
| `cookiesFromBrowser` | 空 | yt-dlp `--cookies-from-browser`：空 / `chrome` / `edge` / `firefox` |
| `cookiesFile` | 空 | 手动导入的 `cookies.txt` 路径 |

---

## 七、使用说明

### 1. 下载并运行

获取 `dist\XDM.exe`（或官网下载），双击直接运行。单文件绿色版，不写注册表、不留垃圾。

- 若浏览器提示「未知发布者」，选择「仍要运行」（程序使用本地自签名证书 `CN=Xlauch`）。
- 缺失 WebView2 时程序会自动用默认浏览器打开界面，功能一致。

### 2. 添加任务

把 HTTP 直链、磁力链接、m3u8 地址或视频页链接粘贴到顶部输入框，回车或点「新建下载 ▾」：

- 「新建下载」「添加」按钮均为下拉菜单，可选择「添加 URL」或「打开 BT 种子…」（限 `.torrent`）。
- 菜单栏「任务(T) → 打开BT种子…」同样可上传种子文件。
- 开启剪贴板监听后，复制链接会自动提示添加。

### 3. 选择画质 / 解析

视频链接（YouTube、B站、抖音等）会自动解析出全部可用画质，勾选清晰度后开始下载；抖音公开作品无需登录。

> 首次解析站点视频会自动下载 yt-dlp 内核（约 17MB）；m3u8 转 MP4 需要 ffmpeg，同样按需下载，全程无需手动干预。

### 4. 管理下载

- 支持暂停 / 恢复 / 重试 / 删除（删除时可选是否同时删除文件）。
- 左侧按「任务状态」「分类」双维度过滤，右键呼出快捷菜单。
- 关闭窗口默认收进系统托盘继续下载；托盘右键可退出。
- 底部状态栏实时显示任务总数、总速度、当前保存路径。

### 5. 浏览器扩展安装（Chrome / Edge）

1. 下载 `xdm-browser-extension.zip`（或在 XDM「帮助 → 插件下载与使用」中一键解压）；
2. 解压到固定目录（如 `d:\xdm-extension`），目录不要删除或移动；
3. 地址栏打开 `chrome://extensions`（Edge 为 `edge://extensions`），开启右上角「开发者模式」；
4. 点击「加载已解压的扩展程序」选择该目录，工具栏出现 XDM 图标即成功，建议固定到工具栏。

> 浏览器安全模型不允许程序静默安装扩展，因此必须走上述手动「加载已解压的扩展程序」流程；XDM 可一键解压并自动打开扩展管理页。

扩展能力：

- **资源嗅探**：播放视频后点击扩展图标，列出本页探测到的视频/音频（显示文件名与 `type · size`），点击即添加到 XDM；鼠标在视频、大图、下载链接上停留约 350ms 会弹出「下载」浮条。
- **右键推送**：链接 / 图片 / 媒体上右键 →「使用 XDM 下载此链接 / 图片 / 媒体」。
- **导出登录 Cookie**：在抖音、B站、YouTube 等已登录页面一键导出（含 `httpOnly`），XDM 解析下载该站点时自动携带登录态。

### 6. 会员 / 登录可见内容

Cookie 来源按优先级自动择优：

1. 浏览器扩展推送；
2. 设置中导入的 `cookies.txt`；
3. 读取本机浏览器 Cookie（Chrome / Edge / Firefox）。

### 7. 常见问题速查

| 问题 | 处理 |
| --- | --- |
| 支持哪些系统？ | Windows 10 / 11 64 位；WebView2 缺失时自动回退浏览器 |
| 是否免费 / 有广告？ | 完全免费、无广告、不收集数据；只监听 `127.0.0.1` |
| 视频存到哪里？ | 默认 `d:\xdm`，可在「设置 → 下载目录」修改，也支持单任务临时目录 |
| 关窗口会中断吗？ | 默认收进托盘继续下载；可在「设置 → 关闭方式」改为直接退出 |
| 抖音解析失败？ | 公开作品免登录；私密/受限作品会明确提示；首次解析较慢，超时可重试 |
| 支持代理吗？ | 支持，「设置 → 代理服务器」填 `http://` 或 `socks5://` 地址 |
| 磁力一直 0 速度？ | 冷门/无做种资源无法下载；热门种子请删除旧任务后重新添加 |

---

## 八、调试工具

`code\tdm\cmd\dhttest`：独立验证 DHT `get_peers` 能否发现指定磁力的 peer（排查 BT 网络问题用）。

```powershell
cd code\tdm
go run ./cmd/dhttest "magnet:?xt=urn:btih:xxxx"   # 不传参数时使用内置测试磁力
```

输出依次为：单节点 ping 通路、单节点 get_peers 返回结构、完整迭代查找耗时与 peer 列表。

BT 端到端测试：

```powershell
cd code\tdm
go test ./internal/bt -run TestMagnetE2E -v -timeout 200s
```

---

## 九、注意事项

- `tools/` 与 `dist/` 均已加入 `.gitignore`；提交前请确认不要 `git add -f` 大文件（GitHub 单文件上限 100 MB）。
- `code\tdm\internal\server\webext\` 是构建生成的副本，扩展源码只改 `code\browser-extension\`。
- 发版时记得：重新构建 → 记录新的 SHA-256 → 同步官网下载区与版本号。
- 抖音解析依赖隐藏的 WebView2 窗口，请勿对窗口加 `WS_EX_LAYERED` 完全透明（Chromium 会节流导致解析/下载为 0）。
