//go:build linux

package program

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func applyWorkerLimits() (map[string]uint64, bool, error) {
	signal.Ignore(syscall.SIGXFSZ)
	limits := map[string]uint64{}
	if err := setLimit(syscall.RLIMIT_CORE, 0); err != nil {
		return limits, false, err
	}
	if err := setLimit(syscall.RLIMIT_FSIZE, LimitFileSize); err != nil {
		return limits, false, err
	}
	limits["fsize"] = readLimit(syscall.RLIMIT_FSIZE)
	if err := setLimit(syscall.RLIMIT_NOFILE, LimitOpenFiles); err != nil {
		return limits, false, err
	}
	limits["nofile"] = readLimit(syscall.RLIMIT_NOFILE)
	if err := setLimit(syscall.RLIMIT_CPU, LimitCPUSeconds); err != nil {
		return limits, false, err
	}
	limits["cpu"] = readLimit(syscall.RLIMIT_CPU)
	if err := setLimit(syscall.RLIMIT_AS, LimitAddressSpace); err != nil {
		return limits, false, err
	}
	limits["as"] = readLimit(syscall.RLIMIT_AS)
	blocked := !canWriteOneByte()
	ok := limits["fsize"] == LimitFileSize && limits["nofile"] == LimitOpenFiles && limits["cpu"] == LimitCPUSeconds && limits["as"] == LimitAddressSpace && blocked
	if !ok {
		return limits, blocked, fmt.Errorf("rlimit readback mismatch: %#v write_blocked=%v", limits, blocked)
	}
	return limits, blocked, nil
}

func setLimit(resource int, value uint64) error {
	r := syscall.Rlimit{Cur: value, Max: value}
	return syscall.Setrlimit(resource, &r)
}

func readLimit(resource int) uint64 {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(resource, &r); err != nil {
		return ^uint64(0)
	}
	return r.Cur
}

func canWriteOneByte() bool {
	f, err := os.Create(filepath.Join(os.TempDir(), "ch-fsize-"+strconv.Itoa(os.Getpid())))
	if err != nil {
		return false
	}
	defer func() {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}()
	_, err = f.Write([]byte("x"))
	return err == nil
}

var cpuSink uint64

func burnCPU() {
	_ = setLimit(syscall.RLIMIT_CPU, 1)
	signal.Reset(syscall.SIGXCPU)
	for {
		cpuSink++
	}
}

func readProcLimits(pid int) (map[string]string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "Max cpu time"):
			out["cpu"] = limitField(line)
		case strings.HasPrefix(line, "Max file size"):
			out["fsize"] = limitField(line)
		case strings.HasPrefix(line, "Max address space"):
			out["as"] = limitField(line)
		case strings.HasPrefix(line, "Max open files"):
			out["nofile"] = limitField(line)
		}
	}
	return out, nil
}

func limitField(line string) string {
	fields := strings.Fields(line)
	// "Max cpu time  5  5  seconds" or "Max address space  N N bytes"
	if len(fields) < 4 {
		return ""
	}
	// The soft limit is the first numeric or "unlimited" field after the name.
	for _, f := range fields {
		if f == "unlimited" || isDigits(f) {
			return f
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readProcEnviron(pid int) (string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(body), "\x00", "\n"), nil
}

func environHasSecrets(env string) bool {
	for _, line := range strings.Split(env, "\n") {
		key, _, _ := strings.Cut(line, "=")
		up := strings.ToUpper(key)
		switch {
		case up == "OPENAI_API_KEY", up == "ANTHROPIC_API_KEY", up == "AWS_SECRET_ACCESS_KEY":
			return true
		case strings.Contains(up, "SECRET"), strings.Contains(up, "PASSWORD"), strings.Contains(up, "TOKEN"):
			return true
		case strings.HasSuffix(up, "_KEY") && up != "CLOUD_HARNESS_WORKER":
			return true
		}
	}
	return false
}

func probeCgroup() (bool, string) {
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false, err.Error()
	}
	line := strings.TrimSpace(string(body))
	_, path, ok := strings.Cut(line, "::")
	if !ok || path == "" {
		return false, "cgroup path missing"
	}
	dir := filepath.Join("/sys/fs/cgroup"+path, "ch-m0-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return false, err.Error()
	}
	defer os.Remove(dir)
	err = os.WriteFile(filepath.Join(dir, "memory.max"), []byte("134217728"), 0o200)
	if err != nil {
		return false, "memory.max: " + err.Error()
	}
	return true, "memory.max writable"
}
