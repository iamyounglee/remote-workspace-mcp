//go:build unix

package auth

import (
	"fmt"
	"os"
)

// enforceOwnerOnly 校验令牌文件未向 group/other 授予任何访问权限。
// 仅 POSIX 系统（Unix/Linux/macOS）有可用权限位，因此该校验仅在这些平台上启用。
func enforceOwnerOnly(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat token file: %w", err)
	}
	if info.Mode().Perm()&0o77 != 0 {
		return fmt.Errorf("token file permissions must not grant group or other access: %s", info.Mode().Perm())
	}
	return nil
}
