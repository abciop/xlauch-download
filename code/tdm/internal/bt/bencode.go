// Package bt 自研 BitTorrent 栈：bencode / DHT(KRPC) / tracker / peer 协议 / 磁力元数据。
// 全部基于 Go 标准库，无第三方依赖。
package bt

import (
	"fmt"
	"sort"
	"strconv"
)

// ---------- 解码 ----------

// Decode 解析 bencode 字节流 → map[string]any / []any / int64 / string
func Decode(b []byte) (any, error) {
	v, n, err := decode(b, 0)
	if err != nil {
		return nil, err
	}
	_ = n
	return v, nil
}

func decode(b []byte, i int) (any, int, error) {
	if i >= len(b) {
		return nil, i, fmt.Errorf("bencode: 截断")
	}
	switch c := b[i]; {
	case c == 'i': // 整数 i123e
		j := i + 1
		for j < len(b) && b[j] != 'e' {
			j++
		}
		if j >= len(b) {
			return nil, j, fmt.Errorf("bencode: 整数无终止")
		}
		n, err := strconv.ParseInt(string(b[i+1:j]), 10, 64)
		if err != nil {
			return nil, j, err
		}
		return n, j + 1, nil
	case c >= '0' && c <= '9': // 字符串 5:hello
		j := i
		for j < len(b) && b[j] != ':' {
			j++
		}
		if j >= len(b) {
			return nil, j, fmt.Errorf("bencode: 字符串无冒号")
		}
		l, err := strconv.Atoi(string(b[i:j]))
		if err != nil || l < 0 || j+1+l > len(b) {
			return nil, j, fmt.Errorf("bencode: 非法字符串长度")
		}
		return string(b[j+1 : j+1+l]), j + 1 + l, nil
	case c == 'l': // 列表 l...e
		out := []any{}
		i++
		for i < len(b) && b[i] != 'e' {
			v, ni, err := decode(b, i)
			if err != nil {
				return nil, ni, err
			}
			out = append(out, v)
			i = ni
		}
		return out, i + 1, nil
	case c == 'd': // 字典 d...e（键必须为字符串，按序排列）
		out := map[string]any{}
		i++
		for i < len(b) && b[i] != 'e' {
			k, ni, err := decode(b, i)
			if err != nil {
				return nil, ni, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, ni, fmt.Errorf("bencode: 字典键非字符串")
			}
			v, ni2, err := decode(b, ni)
			if err != nil {
				return nil, ni2, err
			}
			out[ks] = v
			i = ni2
		}
		return out, i + 1, nil
	}
	return nil, i, fmt.Errorf("bencode: 非法类型字节 %q", b[i])
}

// splitTopKey 从 bencoded 顶层字典中取出 key 的原始 bencoded 值字节（含长度前缀）。
// Decode 会把嵌套字典解析成 map，无法用于计算 info 字典的 sha1，因此需要原始字节。
func splitTopKey(b []byte, key string) ([]byte, error) {
	if len(b) == 0 || b[0] != 'd' {
		return nil, fmt.Errorf("bencode: 非字典")
	}
	i := 1
	for i < len(b) && b[i] != 'e' {
		k, ni, err := decode(b, i)
		if err != nil {
			return nil, err
		}
		ks, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("bencode: 字典键非字符串")
		}
		start := ni
		_, ni2, err := decode(b, ni)
		if err != nil {
			return nil, err
		}
		if ks == key {
			return b[start:ni2], nil
		}
		i = ni2
	}
	return nil, fmt.Errorf("bencode: 缺少键 %q", key)
}

// ---------- 编码 ----------

// Encode 任意值 → bencode 字节流
func Encode(v any) []byte {
	var buf []byte
	buf = encode(buf, v)
	return buf
}

func encode(buf []byte, v any) []byte {
	switch x := v.(type) {
	case int:
		buf = append(buf, 'i')
		buf = strconv.AppendInt(buf, int64(x), 10)
		return append(buf, 'e')
	case int64:
		buf = append(buf, 'i')
		buf = strconv.AppendInt(buf, x, 10)
		return append(buf, 'e')
	case uint32:
		buf = append(buf, 'i')
		buf = strconv.AppendUint(buf, uint64(x), 10)
		return append(buf, 'e')
	case string:
		buf = append(buf, strconv.Itoa(len(x))...)
		buf = append(buf, ':')
		return append(buf, x...)
	case []byte:
		buf = append(buf, strconv.Itoa(len(x))...)
		buf = append(buf, ':')
		return append(buf, x...)
	case []any:
		buf = append(buf, 'l')
		for _, e := range x {
			buf = encode(buf, e)
		}
		return append(buf, 'e')
	case []string:
		buf = append(buf, 'l')
		for _, e := range x {
			buf = encode(buf, e)
		}
		return append(buf, 'e')
	case map[string]any:
		buf = append(buf, 'd')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // bencode 字典必须按键排序
		for _, k := range keys {
			buf = encode(buf, k)
			buf = encode(buf, x[k])
		}
		return append(buf, 'e')
	case map[any]any:
		buf = append(buf, 'd')
		keys := make([]string, 0, len(x))
		conv := map[string]any{}
		for k, v := range x {
			ks := fmt.Sprint(k)
			keys = append(keys, ks)
			conv[ks] = v
		}
		sort.Strings(keys)
		for _, k := range keys {
			buf = encode(buf, k)
			buf = encode(buf, conv[k])
		}
		return append(buf, 'e')
	}
	return append(buf, 'i', '0', 'e')
}
