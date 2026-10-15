package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/Dudude-bit/ccswap/internal/cli"
)

var version = "dev"

// buildVersion is version, or when it is "dev" the module version `go install …@vX` recorded.
func buildVersion(v string, info func() (*debug.BuildInfo, bool)) string {
	if v != "dev" {
		return v
	}
	if bi, ok := info(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return v
}

func main() {
	err := cli.Execute(context.Background(), buildVersion(version, debug.ReadBuildInfo))
	if err == nil {
		return
	}
	var ee *cli.ExitError
	code := 1
	if errors.As(err, &ee) {
		code = ee.Code
	}
	if ee == nil || ee.Err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
