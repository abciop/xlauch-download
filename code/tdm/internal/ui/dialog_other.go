//go:build !windows

package ui

import "fmt"

// PickDirectory 非 Windows 平台不支持系统文件夹选择框。
func PickDirectory(initial string) (string, error) {
	return "", fmt.Errorf("仅支持 Windows")
}

// PickFile 非 Windows 平台不支持系统文件选择框。
func PickFile(initial string) (string, error) {
	return "", fmt.Errorf("仅支持 Windows")
}
