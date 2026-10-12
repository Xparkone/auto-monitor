// Package jutil 提供 map[string]any 的安全读取助手，
// 对应 Python 版随处可见的 dict.get 用法。
package jutil

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Str 读取字符串字段；缺失或类型不符返回空串（对应 Python 的 m.get(key) or ""）。
func Str(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Float 读取数值字段；第二返回值表示字段存在且是数值。
func Float(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// Int 读取整数字段。
func Int(m map[string]any, key string) (int, bool) {
	if f, ok := Float(m, key); ok {
		return int(f), true
	}
	return 0, false
}

// Bool 读取布尔字段。
func Bool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	if b, ok := v.(bool); ok {
		return b, true
	}
	return false, false
}

// Slice 读取数组字段；缺失返回 nil。
func Slice(m map[string]any, key string) []any {
	if v, ok := m[key]; ok {
		if s, ok := v.([]any); ok {
			return s
		}
	}
	return nil
}

// Map 读取嵌套对象字段；缺失返回空 map。
func Map(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return map[string]any{}
}

// MapSlice 读取对象数组字段，过滤掉非对象元素。
func MapSlice(m map[string]any, key string) []map[string]any {
	items := Slice(m, key)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if mm, ok := item.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// StringSlice 读取字符串数组字段，可容忍单字符串。
func StringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return []string{s}
	}
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// PyFloat 按 Python str(float) 规则格式化：整数值浮点渲染为 "80.0"，
// 非整数渲染为最短表示（"85.5"），保持与 Python 版报告文案一致。
func PyFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// PyStr 按 Python str() 规则渲染标量：nil→""、bool→True/False、
// float64→PyFloat、其余用默认格式化（int、string 等与 Python 一致）。
func PyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return PyFloat(x)
	case float32:
		return PyFloat(float64(x))
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return x
	default:
		return ""
	}
}
