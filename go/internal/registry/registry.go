// Package registry 工具注册表：新增巡检工具只需一次 Register 调用
// （对应 Python 版 mcp_inspect/registry.py）。
//
// tools/list、tools/call 都由注册表驱动，新增工具后服务端无需任何改动。
package registry

import "fmt"

type ToolFn func(args map[string]any) map[string]any

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     ToolFn
}

var (
	tools = map[string]*Tool{}
	order []string
)

// Register 把函数注册为 MCP 工具：接收 arguments，返回结构化 JSON（map）。
func Register(name, description string, inputSchema map[string]any, fn ToolFn) {
	if _, exists := tools[name]; !exists {
		order = append(order, name)
	}
	tools[name] = &Tool{Name: name, Description: description, InputSchema: inputSchema, Handler: fn}
}

// ToolSpecs 返回 tools/list 的规格（不含 handler），按注册顺序。
func ToolSpecs() []map[string]any {
	specs := make([]map[string]any, 0, len(order))
	for _, name := range order {
		t := tools[name]
		specs = append(specs, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return specs
}

// Call 按名调用工具；未注册返回错误（对应 Python 的 ValueError）。
func Call(name string, arguments map[string]any) (map[string]any, error) {
	t, ok := tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return t.Handler(arguments), nil
}
