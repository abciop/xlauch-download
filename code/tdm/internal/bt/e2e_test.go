package bt

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"tdm/internal/config"
	"tdm/internal/task"
)

// TestMagnetE2E 真实磁力端到端：DHT 发现 peer → 握手 → ut_metadata 获取元数据 → 开始下载。
// 需要公网可达，耗时较长（最长 150s）。
func TestMagnetE2E(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10&dn=Sintel"
	dir := `d:/deve/workspace/xlauch-space/xlauch-download/dist/e2e-dl`
	os.MkdirAll(dir, 0o755)

	cfg := &config.Config{DownloadDir: dir, BtMaxPeers: 30}
	engine := New(cfg)
	tt := &task.Task{ID: "e2e", Kind: task.KBT, URL: magnet, Dir: dir, Status: task.SRunning, Total: -1}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, tt, func(mut func(t *task.Task)) { mut(tt) }) }()

	for i := 0; i < 30; i++ {
		time.Sleep(5 * time.Second)
		select {
		case err := <-done:
			fmt.Printf("[BT-DEBUG] 引擎提前返回: %v (total=%d)\n", err, tt.Total)
			t.Fatalf("引擎提前返回: %v", err)
		default:
		}
		peers, seeds := tt.Peers, tt.Seeds
		fmt.Printf("[%3ds] total=%-10d done=%-10d prog=%5.1f%% speed=%s peers=%d seeds=%d err=%q\n",
			(i+1)*5, tt.Total, tt.Done, tt.Progress, formatSpeed(tt.Speed), peers, seeds, tt.Error)
		if tt.Total > 0 {
			fmt.Printf(">>> 元数据获取成功! 标题=%q\n", tt.Title)
			if tt.Done > 1024*1024 {
				cancel()
				<-done
				t.Logf("下载正常进行，任务完成")
				return
			}
		}
	}
	cancel()
	<-done
	t.Fatal("150 秒内未完成元数据获取/下载")
}

func formatSpeed(bps float64) string {
	if bps <= 0 {
		return "0"
	}
	return fmt.Sprintf("%.0f KB/s", bps/1024)
}
