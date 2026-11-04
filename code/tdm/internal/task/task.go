// Package task 定义下载任务模型与统一任务管理器（对标 TDM Fast 的 tasks.json 体系）。
package task

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tdm/internal/config"
)

// Kind 任务类型
type Kind string

const (
	KHTTP  Kind = "http"  // 普通多线程 HTTP 下载
	KHLS   Kind = "hls"   // m3u8 视频流
	KBT    Kind = "bt"    // 磁力 / 种子
	KVideo Kind = "video" // yt-dlp 站点视频
)

// Status 任务状态
type Status string

const (
	SQueued  Status = "queued"
	SRunning Status = "running"
	SPaused  Status = "paused"
	SDone    Status = "done"
	SError   Status = "error"
)

// Task 任务快照（并发访问需持有 Manager 锁）
type Task struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	Filename  string    `json:"filename,omitempty"`
	Quality   string    `json:"quality,omitempty"` // video: format 选择
	Status    Status    `json:"status"`
	Total     int64     `json:"total"`   // 字节（未知为 -1）
	Done      int64     `json:"done"`    // 已完成字节
	Speed     float64   `json:"speed"`   // B/s
	ETA       int64     `json:"eta"`     // 秒
	Progress  float64   `json:"progress"` // 0-100
	Peers     int       `json:"peers,omitempty"`
	Seeds     int       `json:"seeds,omitempty"`
	Error     string    `json:"error,omitempty"`
	FilePath  string    `json:"filePath,omitempty"` // 完成后实际落盘路径
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Updater 引擎回调：线程安全地修改任务字段
type Updater func(mutate func(t *Task))

// Engine 任务引擎接口：Run 阻塞执行；ctx 取消时保存进度并尽快返回。
// 返回 nil 表示完成（通过 upd 设置 FilePath），返回 err 表示失败。
type Engine interface {
	Run(ctx context.Context, t *Task, upd Updater) error
}

// Manager 任务管理器
type Manager struct {
	mu      sync.RWMutex
	cfg     *config.Config
	tasks   map[string]*Task
	order   []string
	engine  Engine
	cancels map[string]context.CancelFunc
	dones   map[string]chan struct{} // 引擎 goroutine 退出信号（删除任务前等待句柄释放）
	dirty   map[string]bool
	notify  chan struct{}
}

// NewManager 创建管理器
func NewManager(cfg *config.Config, e Engine) *Manager {
	m := &Manager{
		cfg:     cfg,
		tasks:   make(map[string]*Task),
		engine:  e,
		cancels: make(map[string]context.CancelFunc),
		dones:   make(map[string]chan struct{}),
		dirty:   make(map[string]bool),
		notify:  make(chan struct{}, 1),
	}
	m.load()
	go m.loop()
	return m
}

// Notify 返回变更通知信号（SSE 拉取用）
func (m *Manager) Notify() <-chan struct{} { return m.notify }

// Emit 广播一次任务快照
func (m *Manager) Emit() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		hasDirty := len(m.dirty) > 0
		m.mu.Unlock()
		if hasDirty {
			m.Emit()
		}
	}
}

// NewID 由 URL+时间生成短 ID
func NewID(url string) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%d", url, time.Now().UnixNano())))
	return hex.EncodeToString(h[:])[:12]
}

// Detect 根据输入自动识别任务类型（对标 TDM Fast 的站点识别表）
func Detect(raw string) Kind {
	u := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(u, "magnet:?"):
		return KBT
	case strings.HasSuffix(u, ".torrent"):
		return KBT
	case strings.Contains(u, ".m3u8"), strings.Contains(u, "m3u8?"):
		return KHLS
	}
	// 逆向 TDM Fast 得到的重点站点表 + yt-dlp 主流站点
	sites := []string{
		"youtube.com", "youtu.be", "twitter.com", "x.com", "reddit.com", "redd.it",
		"tiktok.com", "douyin.com", "vimeo.com", "bilibili.com", "b23.tv",
		"facebook.com", "fb.watch", "instagram.com", "twitch.tv", "dailymotion.com",
		"youku.com", "iqiyi.com", "v.qq.com", "weibo.com", "xiaohongshu.com",
		"kuaishou.com", "ixigua.com", "soundcloud.com", "bandcamp.com", "nicovideo.jp",
		"pinterest.com", "t.me", "telegram.me",
	}
	for _, s := range sites {
		if strings.Contains(u, s) {
			return KVideo
		}
	}
	return KHTTP
}

// Add 新建任务并调度（kind 传空串表示自动识别）
func (m *Manager) Add(url, dir, filename, quality string, kind Kind) *Task {
	if kind == "" {
		kind = Detect(url)
	}
	if dir == "" {
		dir = m.cfg.DownloadDir
	}
	t := &Task{
		ID:        NewID(url),
		Kind:      kind,
		URL:       strings.TrimSpace(url),
		Title:     guessTitle(url, filename),
		Dir:       dir,
		Filename:  filename,
		Quality:   quality,
		Status:    SQueued,
		Total:     -1,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.dirty[t.ID] = true
	m.mu.Unlock()
	m.Emit()
	m.dispatch(t.ID)
	return t
}

func guessTitle(url, filename string) string {
	if filename != "" {
		return strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	return ""
}

// dispatch 调度执行：等待并发槽位，运行引擎，处理终止态
func (m *Manager) dispatch(id string) {
	go func() {
		// 等待并发槽位
		for {
			m.mu.Lock()
			running := 0
			for _, tid := range m.order {
				if m.tasks[tid].Status == SRunning {
					running++
				}
			}
			t, ok := m.tasks[id]
			if !ok || t.Status != SQueued {
				m.mu.Unlock()
				return
			}
			if running >= m.cfg.MaxActive {
				m.mu.Unlock()
				select {
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			t.Status = SRunning
			m.dirty[id] = true
			m.mu.Unlock()
			m.Emit()

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			m.mu.Lock()
			m.cancels[id] = cancel
			m.dones[id] = done
			m.mu.Unlock()

			upd := Updater(func(mutate func(t *Task)) {
				m.mu.Lock()
				if cur, ok := m.tasks[id]; ok {
					mutate(cur)
					cur.UpdatedAt = time.Now()
					m.dirty[id] = true
				}
				m.mu.Unlock()
			})
			err := m.engine.Run(ctx, t, upd)

			m.mu.Lock()
			cancel()
			delete(m.cancels, id)
			delete(m.dones, id)
			if cur, ok := m.tasks[id]; ok {
				if err != nil {
					if cur.Status == SRunning {
						cur.Status = SError
						cur.Error = err.Error()
					}
				} else if cur.Status == SRunning {
					cur.Status = SDone
					cur.Speed = 0
					if cur.Total > 0 {
						cur.Done = cur.Total
					}
					cur.Progress = 100
				}
				cur.Speed = 0
				m.dirty[id] = true
			}
			m.mu.Unlock()
			m.persist()
			m.Emit()
			close(done) // 引擎已退出、文件句柄已释放
		}
	}()
}

// Pause 暂停任务：置状态并取消引擎
func (m *Manager) Pause(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || (t.Status != SRunning && t.Status != SQueued) {
		m.mu.Unlock()
		return
	}
	t.Status = SPaused
	t.Speed = 0
	m.dirty[id] = true
	cancel, hasCancel := m.cancels[id]
	m.mu.Unlock()
	if hasCancel {
		cancel()
	}
	m.Emit()
}

// Resume 恢复任务（重新排队调度）
func (m *Manager) Resume(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || t.Status != SPaused {
		m.mu.Unlock()
		return
	}
	t.Status = SQueued
	t.Error = ""
	m.dirty[id] = true
	m.mu.Unlock()
	m.dispatch(id)
}

// Retry 重试失败任务
func (m *Manager) Retry(id string) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || t.Status != SError {
		m.mu.Unlock()
		return
	}
	t.Status = SQueued
	t.Error = ""
	m.dirty[id] = true
	m.mu.Unlock()
	m.dispatch(id)
}

// Remove 删除任务（可选删除文件）
func (m *Manager) Remove(id string, delFiles bool) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	if t.Status == SRunning || t.Status == SQueued {
		t.Status = SPaused // 先置态，令 dispatch 循环退出
	}
	cancel, hasCancel := m.cancels[id]
	done, hasDone := m.dones[id]
	filePath := t.FilePath
	delete(m.tasks, id)
	for i, tid := range m.order {
		if tid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	delete(m.cancels, id)
	delete(m.dones, id)
	m.mu.Unlock()
	if hasCancel {
		cancel()
	}
	if hasDone {
		// 等待引擎退出并释放文件句柄（Windows 下删除被占用文件会静默失败）
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if delFiles && filePath != "" {
		removeTaskFiles(filePath)
	}
	m.persist()
	m.Emit()
}

// removeTaskFiles 删除任务产物：目标文件/目录 + 各引擎的断点状态文件
func removeTaskFiles(path string) {
	_ = os.Remove(path + ".tdm.btstate") // BT 断点状态
	_ = os.Remove(path + ".tdm.state")   // HTTP 分块状态
	_ = os.RemoveAll(path + ".tdm.parts") // HLS 分片临时目录（正常情况引擎已自清理）
	if fi, err := os.Stat(path); err == nil {
		if fi.IsDir() {
			_ = os.RemoveAll(path) // BT 多文件种子是一个目录
		} else {
			_ = os.Remove(path)
		}
	}
}

// Snapshot 返回任务列表（按创建时间正序）
func (m *Manager) Snapshot() []Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			out = append(out, *t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Get 获取单个任务
func (m *Manager) Get(id string) (Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

// Config 暴露配置引用
func (m *Manager) Config() *config.Config { return m.cfg }

// path tasks.json 持久化（对标逆向结果）
func (m *Manager) path() string {
	return filepath.Join(config.DataDir(), "tasks.json")
}

func (m *Manager) persist() {
	m.mu.RLock()
	list := make([]Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			list = append(list, *t)
		}
	}
	m.mu.RUnlock()
	b, _ := json.MarshalIndent(list, "", " ")
	_ = os.MkdirAll(config.DataDir(), 0o755)
	_ = os.WriteFile(m.path(), b, 0o644)
}

func (m *Manager) load() {
	b, err := os.ReadFile(m.path())
	if err != nil {
		return
	}
	var list []Task
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for i := range list {
		t := list[i]
		if t.Status == SRunning || t.Status == SQueued {
			t.Status = SPaused // 重启后置为暂停，由用户恢复
		}
		if t.Progress == 0 && t.Total > 0 {
			t.Progress = float64(t.Done) / float64(t.Total) * 100
		}
		m.tasks[t.ID] = &t
		m.order = append(m.order, t.ID)
	}
}

// PersistNow 立即持久化（退出时调用）
func (m *Manager) PersistNow() { m.persist() }
