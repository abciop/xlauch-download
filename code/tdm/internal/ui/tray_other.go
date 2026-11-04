//go:build !windows

package ui

// 非 Windows 平台的托盘占位实现（Windows 为唯一目标平台）
func installWindowIcon(uintptr) {}

func startTray(uintptr) {}

func stopTray() {}

// Quit 非 Windows 平台占位（无托盘/窗口）
func Quit() {}
