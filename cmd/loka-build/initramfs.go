package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

func buildInitramfs(cfg buildConfig) error {
	outputFile := filepath.Join(cfg.OutputDir, "initramfs.cpio.gz")

	// Skip if output exists.
	if info, err := os.Stat(outputFile); err == nil && info.Size() > 0 {
		cfg.Logger.Info("initramfs already exists, skipping", "path", outputFile, "size_kb", info.Size()/1024)
		cfg.Logger.Info("delete to rebuild", "path", outputFile)
		return nil
	}

	os.MkdirAll(cfg.OutputDir, 0o755)

	projectDir, err := findProjectRoot()
	if err != nil {
		return fmt.Errorf("find project root: %w", err)
	}

	cfg.Logger.Info("building initramfs", "arch", cfg.Arch, "output", outputFile)

	// Create temporary build directory.
	initramfsDir, err := os.MkdirTemp("", "loka-initramfs-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(initramfsDir)

	// Create directory structure.
	for _, d := range []string{
		"bin", "sbin", "dev", "proc", "sys", "mnt", "tmp",
		"usr/local/bin", "etc", "workspace",
	} {
		os.MkdirAll(filepath.Join(initramfsDir, d), 0o755)
	}

	// Download busybox static binary.
	if err := downloadBusybox(cfg, initramfsDir); err != nil {
		return fmt.Errorf("download busybox: %w", err)
	}

	// Create busybox symlinks.
	for _, cmd := range []string{
		"sh", "ash", "cat", "ls", "mkdir", "mount", "umount",
		"ln", "cp", "mv", "rm", "echo", "sleep", "mknod",
		"grep", "sed", "awk", "ip", "ifconfig", "route", "hostname",
	} {
		os.Symlink("busybox", filepath.Join(initramfsDir, "bin", cmd))
	}

	// Copy supervisor if available.
	arch := cfg.Arch
	for _, p := range []string{
		filepath.Join(projectDir, "build", "linux-"+arch, "loka-supervisor"),
		filepath.Join(projectDir, "bin", "linux-"+arch, "loka-supervisor"),
	} {
		if data, err := os.ReadFile(p); err == nil {
			os.WriteFile(filepath.Join(initramfsDir, "usr/local/bin/loka-supervisor"), data, 0o755)
			cfg.Logger.Info("included supervisor", "from", p)
			break
		}
	}

	// Write init script.
	if err := os.WriteFile(filepath.Join(initramfsDir, "init"), []byte(initScript), 0o755); err != nil {
		return fmt.Errorf("write init: %w", err)
	}

	// Create cpio.gz archive.
	cfg.Logger.Info("creating cpio archive")
	absOutput, _ := filepath.Abs(outputFile)
	outFile, err := os.Create(absOutput)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer outFile.Close()

	cmd := exec.Command("sh", "-c", "find . | cpio -H newc -o 2>/dev/null | gzip -9")
	cmd.Dir = initramfsDir
	cmd.Stdout = outFile
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create cpio archive: %w", err)
	}

	info, _ := os.Stat(absOutput)
	cfg.Logger.Info("initramfs build complete",
		"output", outputFile,
		"size_kb", info.Size()/1024)

	return nil
}

func downloadBusybox(cfg buildConfig, initramfsDir string) error {
	busyboxArch := "armv8l"
	if cfg.Arch == "amd64" || cfg.Arch == "x86_64" {
		busyboxArch = "x86_64"
	}

	url := fmt.Sprintf(
		"https://busybox.net/downloads/binaries/1.36.1-defconfig-multiarch-musl/busybox-%s",
		busyboxArch)
	dest := filepath.Join(initramfsDir, "bin", "busybox")

	cfg.Logger.Info("downloading busybox", "arch", busyboxArch)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}

	return os.Chmod(dest, 0o755)
}

const initScript = `#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm
mount -t devpts -o newinstance,ptmxmode=0620 devpts /dev/pts
ln -sf pts/ptmx /dev/ptmx
mount -t tmpfs tmpfs /dev/shm
mount -t tmpfs tmpfs /tmp

# Create /dev/vsock for vsock communication.
mknod /dev/vsock c 10 91 2>/dev/null || true

# Parse kernel command line for virtiofs mounts.
for param in $(cat /proc/cmdline); do
    case "$param" in
        loka.virtiofs=*)
            tag_path="${param#loka.virtiofs=}"
            tag="${tag_path%%:*}"
            mpath="${tag_path#*:}"
            mkdir -p "$mpath"
            mount -t virtiofs "$tag" "$mpath" 2>/dev/null || echo "virtiofs mount failed: $tag -> $mpath"
            ;;
        loka.nlayers=*)
            nlayers="${param#loka.nlayers=}"
            # Mount overlay layers.
            lower=""
            for i in $(seq 0 $((nlayers - 1))); do
                mkdir -p "/layers/$i"
                mount -t virtiofs "layer-$i" "/layers/$i" 2>/dev/null || true
                if [ -z "$lower" ]; then lower="/layers/$i"; else lower="/layers/$i:$lower"; fi
            done
            mkdir -p /upper /work /merged
            mount -t virtiofs upper /upper 2>/dev/null || mount -t tmpfs tmpfs /upper
            mkdir -p /upper/data /upper/work
            mount -t overlay overlay -o "lowerdir=$lower,upperdir=/upper/data,workdir=/upper/work" /merged
            # Set up merged root.
            mkdir -p /merged/dev /merged/proc /merged/sys /merged/tmp /merged/run
            mount -t proc proc /merged/proc
            mount -t sysfs sysfs /merged/sys
            mount -t devtmpfs devtmpfs /merged/dev
            mkdir -p /merged/dev/pts /merged/dev/shm
            mount -t devpts -o newinstance,ptmxmode=0620 devpts /merged/dev/pts
            mount -t tmpfs tmpfs /merged/dev/shm
            mount -t tmpfs tmpfs /merged/tmp
            mount -t tmpfs tmpfs /merged/run
            ln -sf pts/ptmx /merged/dev/ptmx
            mknod /merged/dev/vsock c 10 91 2>/dev/null || true
            # Mount additional virtiofs volumes inside merged root.
            for p2 in $(cat /proc/cmdline); do
                case "$p2" in
                    loka.virtiofs=*)
                        vfs_tag_path="${p2#loka.virtiofs=}"
                        vfs_tag="${vfs_tag_path%%:*}"
                        vfs_mpath="${vfs_tag_path#*:}"
                        mkdir -p "/merged$vfs_mpath"
                        mount -t virtiofs "$vfs_tag" "/merged$vfs_mpath" 2>/dev/null || true
                        ;;
                esac
            done
            # Set up loopback before switch_root (network persists across pivot).
            ip link set lo up 2>/dev/null || true
            exec switch_root /merged /usr/local/bin/loka-supervisor 2>/dev/null || exec chroot /merged /bin/sh
            ;;
    esac
done

# Network setup.
ip link set lo up 2>/dev/null || true
ip link set eth0 up 2>/dev/null || true
ip addr add 10.0.2.15/24 dev eth0 2>/dev/null || true
ip route add default via 10.0.2.2 2>/dev/null || true

# Check for loka.exec= parameter (used by loka-build to run build scripts).
for param in $(cat /proc/cmdline); do
    case "$param" in
        loka.exec=*)
            exec_path="${param#loka.exec=}"
            if [ -x "$exec_path" ]; then
                echo "loka.exec: $exec_path"
                exec "$exec_path"
            else
                echo "loka.exec: $exec_path not found or not executable"
            fi
            ;;
    esac
done

# Start supervisor if available.
if [ -x /usr/local/bin/loka-supervisor ]; then
    exec /usr/local/bin/loka-supervisor
fi

# Fallback: drop to shell.
exec /bin/sh
`
