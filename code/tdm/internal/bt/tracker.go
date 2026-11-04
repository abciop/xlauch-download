package bt

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"time"
)

// announcePeers 向 tracker 声明并获取 peers（HTTP/UDP 自动识别）
func announcePeers(ctx context.Context, trackerURL string, infoHash [20]byte, peerID [20]byte, port int, left uint64) []string {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil
	}
	switch u.Scheme {
	case "http", "https":
		return httpAnnounce(ctx, trackerURL, infoHash, peerID, port, left)
	case "udp":
		return udpAnnounce(ctx, u, infoHash, peerID, port, left)
	}
	return nil
}

func httpAnnounce(ctx context.Context, raw string, infoHash [20]byte, peerID [20]byte, port int, left uint64) []string {
	if port <= 0 {
		port = 6881
	}
	q := url.Values{}
	q.Set("info_hash", string(infoHash[:])) // url.Values 自动转义原始字节
	q.Set("peer_id", string(peerID[:]))
	q.Set("port", fmt.Sprint(port))
	q.Set("uploaded", "0")
	q.Set("downloaded", "0")
	q.Set("left", fmt.Sprint(left))
	q.Set("compact", "1")
	q.Set("event", "started")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if req.URL.RawQuery != "" {
		req.URL.RawQuery += "&" + q.Encode()
	} else {
		req.URL.RawQuery = q.Encode()
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	v, err := Decode(buf.Bytes())
	if err != nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	switch peers := m["peers"].(type) {
	case string: // compact
		for i := 0; i+6 <= len(peers); i += 6 {
			if a := compact2addr(peers[i : i+6]); a != "" {
				out = append(out, a)
			}
		}
	case []any: // 字典模型
		for _, p := range peers {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			ip, _ := pm["ip"].(string)
			port, _ := pm["port"].(int64)
			if ip != "" && port > 0 {
				out = append(out, net.JoinHostPort(ip, fmt.Sprint(port)))
			}
		}
	}
	return out
}

// ---------- UDP tracker (BEP-15) ----------

const udpProtocolID = 0x41727101980

func udpAnnounce(ctx context.Context, u *url.URL, infoHash [20]byte, peerID [20]byte, port int, left uint64) []string {
	addr, err := net.ResolveUDPAddr("udp", u.Host)
	if err != nil {
		return nil
	}
	conn, err := net.DialTimeout("udp", addr.String(), 8*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	uc := conn.(*net.UDPConn)
	deadline := time.Now().Add(12 * time.Second)
	uc.SetDeadline(deadline)

	// 1. connect
	var txID uint32 = rand.Uint32()
	connReq := make([]byte, 16)
	binary.BigEndian.PutUint64(connReq[0:], udpProtocolID)
	binary.BigEndian.PutUint32(connReq[8:], 0) // action=connect
	binary.BigEndian.PutUint32(connReq[12:], txID)
	if _, err := uc.Write(connReq); err != nil {
		return nil
	}
	resp := make([]byte, 2048)
	var connID uint64
	for {
		n, err := uc.Read(resp)
		if err != nil || n < 16 {
			return nil
		}
		if binary.BigEndian.Uint32(resp[0:]) == 0 && binary.BigEndian.Uint32(resp[4:]) == txID {
			connID = binary.BigEndian.Uint64(resp[8:])
			break
		}
	}

	// 2. announce
	txID = rand.Uint32()
	ann := make([]byte, 98)
	binary.BigEndian.PutUint64(ann[0:], connID)
	binary.BigEndian.PutUint32(ann[8:], 1) // action=announce
	binary.BigEndian.PutUint32(ann[12:], txID)
	copy(ann[16:], infoHash[:])
	copy(ann[36:], peerID[:])
	binary.BigEndian.PutUint64(ann[56:], 0)            // downloaded
	binary.BigEndian.PutUint64(ann[64:], left)         // left（剩余字节数，勿写 0，否则被当作 seeder）
	binary.BigEndian.PutUint64(ann[72:], 0)            // uploaded
	binary.BigEndian.PutUint32(ann[80:], 2)            // event=started
	binary.BigEndian.PutUint32(ann[84:], uint32(port)) // port
	if _, err := uc.Write(ann); err != nil {
		return nil
	}
	for {
		n, err := uc.Read(resp)
		if err != nil || n < 20 {
			return nil
		}
		if binary.BigEndian.Uint32(resp[0:]) == 1 && binary.BigEndian.Uint32(resp[4:]) == txID {
			// action=announce
			out := []string{}
			for i := 20; i+6 <= n; i += 6 {
				if a := compact2addr(string(resp[i : i+6])); a != "" {
					out = append(out, a)
				}
			}
			return out
		}
	}
}
