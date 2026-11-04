//go:build !windows

package douyin

import (
	"context"
	"fmt"
)

func fetchDetail(ctx context.Context, raw string) (*detailData, error) {
	return nil, fmt.Errorf("抖音专用解析仅支持 Windows")
}
