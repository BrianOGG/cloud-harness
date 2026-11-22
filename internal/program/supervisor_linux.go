//go:build linux

package program

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func applySysProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return cmd.Process.Kill()
}

func supervise(ctx context.Context, msg wire, invoke Invoker) (Result, error) {
	var result Result
	result.Hash = hashScript(msg.Script)
	result.Probe.GOOS = "linux"
	result.Probe.CgroupMemory, result.Probe.CgroupDetail = probeCgroup()
	if ctx == nil {
		ctx = context.Background()
	}
	exe, args, err := exeFn()
	if err != nil {
		result.Probe.Reason = err.Error()
		return result, err
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = []string{"CLOUD_HARNESS_WORKER=1"}
	if tmp := os.TempDir(); tmp != "" {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	dir, err := os.MkdirTemp("", "ch-worker-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	cmd.Dir = dir
	applySysProc(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return result, err
	}
	var errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, io.LimitReader(stderr, 32<<10))
	}()
	if err := cmd.Start(); err != nil {
		return result, err
	}
	defer func() { _ = killGroup(cmd) }()
	result.Probe.SeparateProcess = cmd.Process != nil && cmd.Process.Pid != os.Getpid()
	in := bufio.NewWriter(stdin)
	reader := bufio.NewReaderSize(stdout, 1<<20)
	hello, rerr := readWire(reader)
	if rerr != nil {
		_ = cmd.Wait()
		wg.Wait()
		return result, domain.E(domain.CodeWorkerCrashed, "worker exited before hello: "+oneLine(errBuf.String()))
	}
	fillProbe(&result.Probe, cmd.Process.Pid, hello)
	ok, why := Decide(result.Probe)
	result.Probe.ProgrammaticEnabled = ok
	result.Probe.Reason = why
	if !ok {
		_ = writeWire(in, wire{Type: "stop"})
		_ = in.Flush()
		_ = killGroup(cmd)
		_ = cmd.Wait()
		wg.Wait()
		return result, disabled(why)
	}
	if err := writeWire(in, msg); err != nil {
		return result, err
	}
	if err := in.Flush(); err != nil {
		return result, err
	}
	if msg.Type == "burn_cpu" {
		waitErr := cmd.Wait()
		wg.Wait()
		if waitErr == nil {
			return result, domain.E(domain.CodeResourceLimit, "cpu limit did not stop the worker")
		}
		return result, waitErr
	}
	if msg.Type == "stop" {
		_, _ = readWire(reader)
		_ = cmd.Wait()
		wg.Wait()
		result.OK = true
		return result, nil
	}
	for {
		incoming, rerr := readWire(reader)
		if rerr != nil {
			waitErr := cmd.Wait()
			wg.Wait()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			detail := oneLine(errBuf.String())
			if waitErr != nil {
				detail = waitErr.Error() + " " + detail
			}
			return result, domain.E(domain.CodeWorkerCrashed, strings.TrimSpace(detail))
		}
		switch incoming.Type {
		case "invoke":
			reply := wire{Type: "invoke_result", ID: incoming.ID}
			if invoke == nil {
				reply.Error = domain.E(domain.CodeAuthDenied, "no broker is attached")
			} else {
				ok, data, callErr := invoke(ctx, incoming.Tool, incoming.Args)
				reply.Success = ok
				reply.Data = data
				reply.Error = callErr
			}
			if err := writeWire(in, reply); err != nil {
				return result, err
			}
			if err := in.Flush(); err != nil {
				return result, err
			}
		case "done":
			result.OK = incoming.Success
			result.Data = incoming.Data
			result.Error = incoming.Error
			_ = cmd.Wait()
			wg.Wait()
			return result, nil
		default:
			return result, domain.E(domain.CodeScriptError, "unexpected worker message "+incoming.Type)
		}
	}
}

func fillProbe(p *Probe, pid int, hello wire) {
	p.FsizeWriteBlocked = hello.FsizeWriteBlocked
	p.RlimitAS = hello.Limits["as"] == LimitAddressSpace
	p.RlimitCPU = hello.Limits["cpu"] == LimitCPUSeconds
	p.RlimitFsize = hello.Limits["fsize"] == LimitFileSize
	p.RlimitNofile = hello.Limits["nofile"] == LimitOpenFiles
	observed, err := readProcLimits(pid)
	parentOK := err == nil &&
		observed["as"] == strconv.FormatUint(LimitAddressSpace, 10) &&
		observed["cpu"] == strconv.FormatUint(LimitCPUSeconds, 10) &&
		observed["fsize"] == "0" &&
		observed["nofile"] == strconv.FormatUint(LimitOpenFiles, 10)
	p.ParentVerified = parentOK && hello.LimitsOK && p.RlimitAS && p.RlimitCPU && p.RlimitFsize && p.RlimitNofile
	env, err := readProcEnviron(pid)
	p.SecretEnvBlocked = err == nil && !environHasSecrets(env)
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
