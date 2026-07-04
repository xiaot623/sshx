package sshcompat

import "strings"

var optionsWithRequiredValue = map[string]bool{
	"-B": true, "-b": true, "-c": true, "-D": true, "-E": true, "-e": true,
	"-F": true, "-I": true, "-i": true, "-J": true, "-L": true, "-l": true,
	"-m": true, "-O": true, "-o": true, "-p": true, "-Q": true, "-R": true,
	"-S": true, "-W": true, "-w": true,
}

type Parsed struct {
	Args          []string
	Target        string
	TargetIndex   int
	RemoteCommand []string
	NoWrap        bool
	InfoMode      bool
}

func Parse(args []string) Parsed {
	out := Parsed{Args: make([]string, 0, len(args)), TargetIndex: -1}
	for _, arg := range args {
		if arg == "--no-wrap" {
			out.NoWrap = true
			continue
		}
		out.Args = append(out.Args, arg)
	}

	for i := 0; i < len(out.Args); i++ {
		arg := out.Args[i]
		if arg == "--" {
			if i+1 < len(out.Args) {
				out.Target = out.Args[i+1]
				out.TargetIndex = i + 1
				out.RemoteCommand = append([]string(nil), out.Args[i+2:]...)
			}
			break
		}
		if isInfoMode(arg) {
			out.InfoMode = true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			out.Target = arg
			out.TargetIndex = i
			out.RemoteCommand = append([]string(nil), out.Args[i+1:]...)
			break
		}
		if optionsWithRequiredValue[arg] && i+1 < len(out.Args) {
			if arg == "-Q" {
				out.InfoMode = true
			}
			i++
			continue
		}
		if consumesAttachedValue(arg) {
			continue
		}
	}
	return out
}

func isInfoMode(arg string) bool {
	return arg == "-V" || arg == "-G" || arg == "-Q" || strings.HasPrefix(arg, "-Q")
}

func consumesAttachedValue(arg string) bool {
	if len(arg) < 3 || !strings.HasPrefix(arg, "-") {
		return false
	}
	return optionsWithRequiredValue[arg[:2]]
}
