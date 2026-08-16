package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	currentInstallRoot     = "/etc/gboard-node"
	legacyInstallRoot      = "/etc/xboard-node"
	currentBinaryPath      = "/usr/local/bin/gboard-node"
	legacyBinaryPath       = "/usr/local/bin/xboard-node"
	currentCLIPath         = "/usr/local/bin/gbctl"
	legacyCLIPath          = "/usr/local/bin/xbctl"
	currentCLIPathUsrBin   = "/usr/bin/gbctl"
	legacyCLIPathUsrBin    = "/usr/bin/xbctl"
	currentServiceName     = "gboard-node.service"
	legacyServiceName      = "xboard-node.service"
	currentServiceFilePath = "/etc/systemd/system/gboard-node.service"
	legacyServiceFilePath  = "/etc/systemd/system/xboard-node.service"
	legacyPathToken        = "/etc/xboard-node"
	currentPathToken       = "/etc/gboard-node"
)

func applyCurrentLayout() {
	defaultInstallRoot = currentInstallRoot
	defaultConfigPath = filepath.Join(currentInstallRoot, "config.yml")
	defaultMetaPath = filepath.Join(currentInstallRoot, "install-meta.json")
	defaultCredentialsPath = filepath.Join(currentInstallRoot, "credentials.env")
	defaultBinaryPath = currentBinaryPath
	defaultCLIPath = currentCLIPath
	serviceName = currentServiceName
	serviceFilePath = currentServiceFilePath
}

func applyLegacyLayout() {
	defaultInstallRoot = legacyInstallRoot
	defaultConfigPath = filepath.Join(legacyInstallRoot, "config.yml")
	defaultMetaPath = filepath.Join(legacyInstallRoot, "install-meta.json")
	defaultCredentialsPath = filepath.Join(legacyInstallRoot, "credentials.env")
	defaultBinaryPath = legacyBinaryPath
	defaultCLIPath = legacyCLIPath
	serviceName = legacyServiceName
	serviceFilePath = legacyServiceFilePath
}

func preferCurrentLayout() bool {
	return dirExists(currentInstallRoot) || fileExists(filepath.Join(currentInstallRoot, "config.yml"))
}

func hasLegacyLayout() bool {
	return dirExists(legacyInstallRoot) || fileExists(filepath.Join(legacyInstallRoot, "config.yml"))
}

func usingLegacyLayout() bool {
	return defaultInstallRoot == legacyInstallRoot
}

// resolveLayout prefers /etc/gboard-node when it exists. If only the
// xboard-node tree is present (and we cannot migrate), fall back to it.
func resolveLayout() {
	if preferCurrentLayout() {
		applyCurrentLayout()
		return
	}
	if hasLegacyLayout() {
		applyLegacyLayout()
		return
	}
	applyCurrentLayout()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func rewriteLegacyInstallPaths(s string) string {
	return strings.ReplaceAll(s, legacyPathToken, currentPathToken)
}

func shouldRewriteFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yml", ".yaml", ".json", ".env":
		return true
	}
	return name == "config.yml" || name == "install-meta.json" || name == "credentials.env"
}

func rewriteLegacyFiles(root string) error {
	if !dirExists(root) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Leave historical backups untouched.
			if d.Name() == "backups" && filepath.Dir(path) == root {
				return fs.SkipDir
			}
			return nil
		}
		if !shouldRewriteFile(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated := rewriteLegacyInstallPaths(string(data))
		if updated == string(data) {
			return nil
		}
		info, statErr := d.Info()
		perm := os.FileMode(0o600)
		if statErr == nil {
			perm = info.Mode().Perm()
		}
		return os.WriteFile(path, []byte(updated), perm)
	})
}

// migrateInstallRoot moves oldRoot → newRoot when newRoot is absent.
// If both exist, newRoot is preferred and oldRoot is left intact.
func migrateInstallRoot(oldRoot, newRoot string) (moved bool, err error) {
	oldOK := dirExists(oldRoot)
	newOK := dirExists(newRoot) || fileExists(newRoot)
	if oldOK && !newOK {
		if err := os.Rename(oldRoot, newRoot); err != nil {
			if copyErr := copyDir(oldRoot, newRoot); copyErr != nil {
				return false, fmt.Errorf("migrate %s -> %s: %w", oldRoot, newRoot, copyErr)
			}
			if err := os.RemoveAll(oldRoot); err != nil {
				return false, fmt.Errorf("remove %s after copy: %w", oldRoot, err)
			}
		}
		if err := os.Chmod(newRoot, 0o700); err != nil && !os.IsNotExist(err) {
			return true, err
		}
		if err := rewriteLegacyFiles(newRoot); err != nil {
			return true, err
		}
		return true, nil
	}
	if newOK {
		if err := rewriteLegacyFiles(newRoot); err != nil {
			return false, err
		}
	}
	return false, nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func forceSymlink(oldname, newname string) error {
	if err := os.Remove(newname); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(oldname, newname)
}

func installCLICompatSymlinks() {
	if !fileExists(currentCLIPath) {
		return
	}
	_ = forceSymlink(currentCLIPath, currentCLIPathUsrBin)
	// One-release compatibility: xbctl → gbctl
	_ = forceSymlink(currentCLIPath, legacyCLIPath)
	_ = forceSymlink(currentCLIPath, legacyCLIPathUsrBin)
}

func promoteLegacyBinary() {
	if !fileExists(currentBinaryPath) && fileExists(legacyBinaryPath) {
		_ = copyFile(legacyBinaryPath, currentBinaryPath)
	}
	if fileExists(currentCLIPath) {
		return
	}
	if exe, err := os.Executable(); err == nil && fileExists(exe) {
		_ = copyFile(exe, currentCLIPath)
		return
	}
	if fileExists(legacyCLIPath) && !isSymlink(legacyCLIPath) {
		_ = copyFile(legacyCLIPath, currentCLIPath)
		return
	}
	if fileExists(legacyCLIPathUsrBin) && !isSymlink(legacyCLIPathUsrBin) {
		_ = copyFile(legacyCLIPathUsrBin, currentCLIPath)
	}
}

func isSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

func systemctlQuiet(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func systemctlIsActive(unit string) bool {
	return systemctlQuiet("is-active", "--quiet", unit) == nil
}

func retireLegacyRuntime() error {
	promoteLegacyBinary()

	legacyActive := systemctlIsActive(legacyServiceName)
	if fileExists(legacyServiceFilePath) || legacyActive {
		_ = systemctlQuiet("stop", legacyServiceName)
		_ = systemctlQuiet("disable", legacyServiceName)
	}

	if !fileExists(currentServiceFilePath) && fileExists(currentBinaryPath) && fileExists(filepath.Join(currentInstallRoot, "config.yml")) {
		applyCurrentLayout()
		if err := regenerateServiceFile(); err != nil {
			return fmt.Errorf("write %s: %w", currentServiceFilePath, err)
		}
	}

	if fileExists(currentServiceFilePath) {
		_ = systemctlQuiet("daemon-reload")
		_ = systemctlQuiet("enable", currentServiceName)
		if legacyActive {
			_ = systemctlQuiet("reset-failed", currentServiceName)
			_ = systemctlQuiet("start", currentServiceName)
		}
	}

	if fileExists(legacyServiceFilePath) {
		_ = os.Remove(legacyServiceFilePath)
		_ = systemctlQuiet("daemon-reload")
	}
	if fileExists(legacyBinaryPath) && legacyBinaryPath != currentBinaryPath {
		_ = os.Remove(legacyBinaryPath)
	}
	installCLICompatSymlinks()
	applyCurrentLayout()
	return nil
}

func migrateLegacyInstall(withRuntime bool) error {
	legacyOnly := dirExists(legacyInstallRoot) && !(dirExists(currentInstallRoot) || fileExists(currentInstallRoot))
	if legacyOnly && systemctlIsActive(legacyServiceName) {
		_ = systemctlQuiet("stop", legacyServiceName)
	}
	moved, err := migrateInstallRoot(legacyInstallRoot, currentInstallRoot)
	if err != nil {
		return err
	}
	if moved {
		fmt.Printf("Migrated %s -> %s\n", legacyInstallRoot, currentInstallRoot)
	} else if dirExists(legacyInstallRoot) && dirExists(currentInstallRoot) {
		fmt.Printf("Both %s and %s exist; using %s and leaving the old tree in place\n", currentInstallRoot, legacyInstallRoot, currentInstallRoot)
	}
	applyCurrentLayout()
	if withRuntime {
		return retireLegacyRuntime()
	}
	return nil
}

// prepareInstallLayout resolves old/new paths. When running as root it
// migrates an xboard-node host onto gboard-node names.
func prepareInstallLayout(withRuntime bool) error {
	if os.Geteuid() == 0 {
		if err := migrateLegacyInstall(withRuntime); err != nil {
			resolveLayout()
			return err
		}
		return nil
	}
	resolveLayout()
	return nil
}
