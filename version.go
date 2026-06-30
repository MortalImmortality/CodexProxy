package main

import (
	"fmt"
	"runtime"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
	}
	if commit != "" {
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		v += " (" + short + ")"
	}
	if date != "" {
		v += " built " + date
	}
	return v
}

func printVersion() {
	fmt.Printf("codex-proxy %s %s/%s\n", versionString(), runtime.GOOS, runtime.GOARCH)
}
