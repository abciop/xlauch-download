//go:build windows

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// 通过 PowerShell 调起系统文件夹选择对话框。
// 结果写入临时文件（UTF-8），避免控制台编码影响中文路径。
const pickDirScript = `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
$dlg = New-Object System.Windows.Forms.FolderBrowserDialog
$dlg.Description = '选择下载目录'
$dlg.ShowNewFolderButton = $true
if ($env:TDM_PICK_INITIAL -and (Test-Path -LiteralPath $env:TDM_PICK_INITIAL)) {
  $dlg.SelectedPath = $env:TDM_PICK_INITIAL
}
if ($dlg.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
  [System.IO.File]::WriteAllText($env:TDM_PICK_OUT, $dlg.SelectedPath)
}
`

var pickMu sync.Mutex

// 通过 PowerShell 调起系统文件选择对话框（cookies.txt 等导入用）。
const pickFileScript = `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
$dlg = New-Object System.Windows.Forms.OpenFileDialog
$dlg.Title = 'Select cookies.txt'
$dlg.Filter = 'cookies.txt|*.txt|All files|*.*'
if ($env:TDM_PICK_INITIAL -and (Test-Path -LiteralPath $env:TDM_PICK_INITIAL)) {
  $dlg.InitialDirectory = $env:TDM_PICK_INITIAL
}
if ($dlg.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
  [System.IO.File]::WriteAllText($env:TDM_PICK_OUT, $dlg.FileName)
}
`

// PickFile 弹出文件选择框，返回所选文件；用户取消时返回空字符串。
func PickFile(initial string) (string, error) {
	pickMu.Lock()
	defer pickMu.Unlock()

	f, err := os.CreateTemp("", "tdm-pickf-*.txt")
	if err != nil {
		return "", err
	}
	outPath := f.Name()
	f.Close()
	defer os.Remove(outPath)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA",
		"-WindowStyle", "Hidden", "-Command", pickFileScript)
	cmd.Env = append(os.Environ(),
		"TDM_PICK_INITIAL="+initial,
		"TDM_PICK_OUT="+outPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if b, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// PickDirectory 弹出文件夹选择框，返回所选目录；用户取消时返回空字符串。
func PickDirectory(initial string) (string, error) {
	pickMu.Lock() // 避免重复弹框
	defer pickMu.Unlock()

	f, err := os.CreateTemp("", "tdm-pick-*.txt")
	if err != nil {
		return "", err
	}
	outPath := f.Name()
	f.Close()
	defer os.Remove(outPath)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA",
		"-WindowStyle", "Hidden", "-Command", pickDirScript)
	cmd.Env = append(os.Environ(),
		"TDM_PICK_INITIAL="+initial,
		"TDM_PICK_OUT="+outPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // 不创建控制台窗口
	if b, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}

	b, err := os.ReadFile(outPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
