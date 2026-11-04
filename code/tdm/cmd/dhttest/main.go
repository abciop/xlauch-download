// dhttest：独立验证 DHT get_peers 能否找到指定磁力的 peer（诊断用）
package main

import (
	"fmt"
	"os"
	"time"

	"tdm/internal/bt"
)

func main() {
	magnet := "magnet:?xt=urn:btih:3f9f9a6cdbb94adf66863496f431530d95f48ed2&dn=fr187.2160p"
	if len(os.Args) > 1 {
		magnet = os.Args[1]
	}
	ih, name, _, ok := bt.ParseMagnet(magnet)
	if !ok {
		fmt.Println("磁力解析失败")
		os.Exit(1)
	}
	fmt.Println("name:", name, "infohash:", fmt.Sprintf("%x", ih))

	d, err := bt.NewDHT()
	if err != nil {
		fmt.Println("UDP 绑定失败:", err)
		os.Exit(1)
	}
	defer d.Close()

	// 第一步：单节点 KRPC ping 验证通路
	if r, err := d.Query("dht.transmissionbt.com:6881", "ping", map[string]any{"id": string(make([]byte, 20))}, 3*time.Second); err != nil {
		fmt.Println("[1] ping transmission 失败:", err)
	} else {
		fmt.Println("[1] ping transmission OK, 响应键:", len(r))
	}

	// 第二步：单节点 get_peers 看返回结构
	if r, err := d.Query("dht.transmissionbt.com:6881", "get_peers", map[string]any{"id": string(make([]byte, 20)), "info_hash": string(ih[:])}, 4*time.Second); err != nil {
		fmt.Println("[2] get_peers transmission 失败:", err)
	} else {
		if vals, ok := r["values"].([]any); ok {
			fmt.Println("[2] 直接返回 peers 数:", len(vals))
		} else {
			fmt.Println("[2] 无 values（本节点不持有该 infohash 的 peer，正常）")
		}
		if ns, ok := r["nodes"].(string); ok {
			fmt.Println("[2] 返回 DHT 节点紧凑串字节数:", len(ns), "(", len(ns)/26, "个节点 )")
		} else {
			fmt.Println("[2] 警告：无 nodes 字段，键:", r)
		}
	}

	// 第三步：完整迭代查找
	start := time.Now()
	peers := d.GetPeers(ih, 8, 60*time.Second, nil)
	fmt.Printf("[3] 完整迭代耗时 %.1fs, 找到 %d 个 peers:\n", time.Since(start).Seconds(), len(peers))
	for _, p := range peers {
		fmt.Println("  ", p)
	}
}
