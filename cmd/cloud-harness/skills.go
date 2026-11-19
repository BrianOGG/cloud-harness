package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	contextpkg "github.com/cloud-harness/cloud-harness/internal/context"
)

const skillUsage = "usage: cloud-harness /skill NAME [--workspace dir]"

func cmdSkill(args []string, stdout, stderr io.Writer) int {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, skillUsage) }
	workspace := fs.String("workspace", ".", "workspace root for .agents/skills")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if name == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	return printSkill(*workspace, name, stdout, stderr)
}

func runChatSkillCommand(workspace, input string, stdout, stderr io.Writer) (bool, int) {
	fields := strings.Fields(input)
	if len(fields) == 0 || fields[0] != "/skill" {
		return false, 0
	}
	if len(fields) != 2 {
		fmt.Fprintln(stderr, "usage: /skill NAME")
		return true, 2
	}
	return true, printSkill(workspace, fields[1], stdout, stderr)
}

func printSkill(workspace, name string, stdout, stderr io.Writer) int {
	skill, err := contextpkg.LoadSkill(workspace, name)
	if err == nil {
		_, err = io.WriteString(stdout, skill.Content)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
