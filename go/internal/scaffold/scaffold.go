// Package scaffold 实现 `auto-monitor init`：在指定目录生成运行所需的
// 配置、skills 规则说明书与目录骨架。模板用 go:embed 内嵌在二进制里，
// 运行时读写的始终是磁盘上的文件，用户可自由修改。
package scaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed assets/config/inspection.toml assets/skills/k8s-pods.md assets/skills/middleware.md assets/skills/server-resources.md
var assets embed.FS

var dirs = []string{"reports", "logs", "data"}

// Init 在 dir 下生成脚手架。已存在的文件默认跳过；force 为 true 时覆盖。
// log 用于回显每个文件/目录的动作（created / skipped）。
func Init(dir string, force bool, log func(string)) error {
	if log == nil {
		log = func(string) {}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("创建目录失败 %s: %w", abs, err)
	}

	written, skipped := 0, 0
	err = fs.WalkDir(assets, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("assets", path)
		if err != nil {
			return err
		}
		target := filepath.Join(abs, rel)
		if _, err := os.Stat(target); err == nil && !force {
			log(fmt.Sprintf("跳过（已存在）: %s", target))
			skipped++
			return nil
		}
		body, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("写入失败 %s: %w", target, err)
		}
		log(fmt.Sprintf("生成: %s", target))
		written++
		return nil
	})
	if err != nil {
		return err
	}

	for _, name := range dirs {
		target := filepath.Join(abs, name)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("创建目录失败 %s: %w", target, err)
		}
		log(fmt.Sprintf("目录: %s", target))
	}

	log(fmt.Sprintf("完成：%d 个文件生成，%d 个跳过。目录已就绪于 %s", written, skipped, abs))
	if written > 0 {
		log("下一步：编辑 config/inspection.toml，然后 `auto-monitor server -c " +
			filepath.Join(abs, "config", "inspection.toml") + "`")
	}
	return nil
}
