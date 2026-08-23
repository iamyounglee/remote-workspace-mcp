//go:build windows

package auth

// enforceOwnerOnly 在 Windows 上为空操作。
// Windows 使用 ACL 而非 POSIX 权限位，os.FileMode.Perm() 无法表达所有者独占访问，
// 因此在 Windows 上跳过该权限校验，避免测试与运行时的误报。
func enforceOwnerOnly(path string) error {
	return nil
}
