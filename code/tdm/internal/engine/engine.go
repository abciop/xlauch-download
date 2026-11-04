// Package engine 汇总各下载引擎并按任务类型分发。
package engine

import (
	"context"

	"tdm/internal/bt"
	"tdm/internal/config"
	"tdm/internal/douyin"
	"tdm/internal/hls"
	"tdm/internal/httpdl"
	"tdm/internal/task"
	"tdm/internal/ytdlp"
)

// MultiEngine 多引擎路由
type MultiEngine struct {
	http  *httpdl.Engine
	hls   *hls.Engine
	bt    *bt.Engine
	video *ytdlp.Engine
	dy    *douyin.Engine
}

// New 创建多引擎路由
func New(cfg *config.Config) *MultiEngine {
	h := httpdl.New(cfg)
	return &MultiEngine{
		http:  h,
		hls:   hls.New(cfg),
		bt:    bt.New(cfg),
		video: ytdlp.New(cfg),
		dy:    douyin.New(cfg, h),
	}
}

// Run 实现 task.Engine 接口
func (m *MultiEngine) Run(ctx context.Context, t *task.Task, upd task.Updater) error {
	switch t.Kind {
	case task.KHLS:
		return m.hls.Run(ctx, t, upd)
	case task.KBT:
		return m.bt.Run(ctx, t, upd)
	case task.KVideo:
		if douyin.IsURL(t.URL) {
			return m.dy.Run(ctx, t, upd)
		}
		return m.video.Run(ctx, t, upd)
	default:
		return m.http.Run(ctx, t, upd)
	}
}
