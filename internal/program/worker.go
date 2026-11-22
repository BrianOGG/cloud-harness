package program

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// starlarkEntries counts interpreter entries in this process. The parent
// process must stay at zero; only the worker command increments it.
var starlarkEntries int

type wire struct {
	Type              string            `json:"type"`
	PID               int               `json:"pid,omitempty"`
	Limits            map[string]uint64 `json:"limits,omitempty"`
	LimitsOK          bool              `json:"limits_ok"`
	FsizeWriteBlocked bool              `json:"fsize_write_blocked"`
	Success           bool              `json:"success"`
	Error             *domain.Error     `json:"error,omitempty"`
	Script            string            `json:"script,omitempty"`
	StepLimit         uint64            `json:"step_limit,omitempty"`
	ID                string            `json:"id,omitempty"`
	Tool              string            `json:"tool,omitempty"`
	Args              json.RawMessage   `json:"args,omitempty"`
	Data              json.RawMessage   `json:"data,omitempty"`
}

// ServeWorker is the child entry point. It applies limits before reading a
// script and refuses to interpret anything when those limits did not stick.
func ServeWorker(in io.Reader, out io.Writer) {
	reader := bufio.NewReaderSize(in, 1<<20)
	limits, blocked, err := applyWorkerLimits()
	hello := wire{Type: "hello", PID: os.Getpid(), Limits: limits, LimitsOK: err == nil, FsizeWriteBlocked: blocked}
	if err != nil {
		hello.Error = domain.E(domain.CodeProgramDisabled, err.Error())
	}
	if werr := writeWire(out, hello); werr != nil {
		return
	}
	msg, rerr := readWire(reader)
	if rerr != nil {
		return
	}
	if !hello.LimitsOK {
		_ = writeWire(out, wire{Type: "done", Success: false, Error: domain.E(domain.CodeProgramDisabled, "worker limits were not established")})
		return
	}
	switch msg.Type {
	case "stop":
		_ = writeWire(out, wire{Type: "done", Success: true})
	case "burn_cpu":
		burnCPU()
	case "exec":
		_ = writeWire(out, runStarlark(msg, reader, out))
	default:
		_ = writeWire(out, wire{Type: "done", Success: false, Error: domain.E(domain.CodeScriptPolicy, "unknown worker command")})
	}
}

func runStarlark(msg wire, in io.Reader, out io.Writer) wire {
	starlarkEntries++
	if len(msg.Script) > MaxScriptBytes {
		return wire{Type: "done", Success: false, Error: domain.E(domain.CodeScriptPolicy, "script exceeds the size limit")}
	}
	steps := msg.StepLimit
	if steps == 0 {
		steps = DefaultStepLimit
	}
	if steps > MaxStepLimit {
		steps = MaxStepLimit
	}
	var seq int
	invoke := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var name string
		var dict *starlark.Dict
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name, "args?", &dict); err != nil {
			return nil, err
		}
		payload, err := dictToJSON(dict)
		if err != nil {
			return nil, err
		}
		seq++
		if err := writeWire(out, wire{Type: "invoke", ID: strconv.Itoa(seq), Tool: name, Args: payload}); err != nil {
			return nil, err
		}
		reply, err := readWire(in)
		if err != nil {
			return nil, err
		}
		return replyToStarlark(reply), nil
	}
	thread := &starlark.Thread{
		Name:  "worker",
		Print: func(*starlark.Thread, string) {},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("module loading is disabled")
		},
	}
	thread.SetMaxExecutionSteps(steps)
	predeclared := starlark.StringDict{
		"broker": &starlarkstruct.Module{Name: "broker", Members: starlark.StringDict{
			"invoke": starlark.NewBuiltin("broker.invoke", invoke),
		}},
	}
	opts := &syntax.FileOptions{}
	globals, err := starlark.ExecFileOptions(opts, thread, "script.star", msg.Script, predeclared)
	if err != nil {
		return wire{Type: "done", Success: false, Error: scriptErr(err)}
	}
	main, ok := globals["main"]
	if !ok {
		return wire{Type: "done", Success: false, Error: domain.E(domain.CodeScriptPolicy, "script must define main()")}
	}
	val, err := starlark.Call(thread, main, nil, nil)
	if err != nil {
		return wire{Type: "done", Success: false, Error: scriptErr(err)}
	}
	raw, err := starlarkToJSON(val)
	if err != nil {
		return wire{Type: "done", Success: false, Error: domain.E(domain.CodeScriptError, err.Error())}
	}
	return wire{Type: "done", Success: true, Data: raw}
}

func scriptErr(err error) *domain.Error {
	var syn syntax.Error
	if errors.As(err, &syn) {
		return domain.E(domain.CodeScriptSyntax, syn.Error())
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "too many steps"):
		return domain.E(domain.CodeResourceLimit, msg)
	case strings.Contains(msg, "module loading is disabled"):
		return domain.E(domain.CodeScriptPolicy, msg)
	default:
		return domain.E(domain.CodeScriptError, msg)
	}
}

func writeWire(out io.Writer, msg wire) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = out.Write(raw)
	if f, ok := out.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	return err
}

func readWire(in io.Reader) (wire, error) {
	r, ok := in.(*bufio.Reader)
	if !ok {
		r = bufio.NewReaderSize(in, 1<<20)
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		return wire{}, err
	}
	if len(line) > 1<<20 {
		return wire{}, fmt.Errorf("worker message exceeds 1MiB")
	}
	var msg wire
	if err := json.Unmarshal(line, &msg); err != nil {
		return wire{}, err
	}
	return msg, nil
}

func dictToJSON(d *starlark.Dict) (json.RawMessage, error) {
	if d == nil {
		return json.RawMessage(`{}`), nil
	}
	v, err := starlarkToGo(d)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func starlarkToJSON(v starlark.Value) (json.RawMessage, error) {
	g, err := starlarkToGo(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

func starlarkToGo(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.String:
		return string(x), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		return x.String(), nil
	case starlark.Float:
		return float64(x), nil
	case *starlark.List:
		var out []any
		for item := range x.Elements() {
			g, err := starlarkToGo(item)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			key, ok := item[0].(starlark.String)
			if !ok {
				return nil, errors.New("dict key must be a string")
			}
			g, err := starlarkToGo(item[1])
			if err != nil {
				return nil, err
			}
			out[string(key)] = g
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported starlark type %s", v.Type())
	}
}

func replyToStarlark(msg wire) starlark.Value {
	d := starlark.NewDict(3)
	_ = d.SetKey(starlark.String("ok"), starlark.Bool(msg.Success))
	data, _ := jsonToStarlark(msg.Data)
	if data == nil {
		data = starlark.None
	}
	_ = d.SetKey(starlark.String("data"), data)
	if msg.Error != nil {
		ed := starlark.NewDict(2)
		_ = ed.SetKey(starlark.String("code"), starlark.String(msg.Error.Code))
		_ = ed.SetKey(starlark.String("message"), starlark.String(msg.Error.Message))
		_ = d.SetKey(starlark.String("error"), ed)
	} else {
		_ = d.SetKey(starlark.String("error"), starlark.None)
	}
	return d
}

func jsonToStarlark(raw json.RawMessage) (starlark.Value, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return starlark.None, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return goToStarlark(v)
}

func goToStarlark(v any) (starlark.Value, error) {
	switch x := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(x), nil
	case string:
		return starlark.String(x), nil
	case float64:
		if x == float64(int64(x)) {
			return starlark.MakeInt64(int64(x)), nil
		}
		return starlark.Float(x), nil
	case []any:
		list := starlark.NewList(nil)
		for _, item := range x {
			sv, err := goToStarlark(item)
			if err != nil {
				return nil, err
			}
			if err := list.Append(sv); err != nil {
				return nil, err
			}
		}
		return list, nil
	case map[string]any:
		d := starlark.NewDict(len(x))
		for k, item := range x {
			sv, err := goToStarlark(item)
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return d, nil
	default:
		return nil, fmt.Errorf("unsupported json type %T", v)
	}
}
