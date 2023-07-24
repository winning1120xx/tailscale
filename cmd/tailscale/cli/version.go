// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/version"
)

var versionCmd = &ffcli.Command{
	Name:       "version",
	ShortUsage: "version [flags]",
	ShortHelp:  "Print Tailscale version",
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("version")
		fs.BoolVar(&versionArgs.daemon, "daemon", false, "also print local node's daemon version")
		fs.BoolVar(&versionArgs.json, "json", false, "output in JSON format")
		fs.BoolVar(&versionArgs.withLatest, "with-latest", false, "include latest released version output")
		return fs
	})(),
	Exec: runVersion,
}

var versionArgs struct {
	daemon     bool // also check local node's daemon version
	json       bool
	withLatest bool
}

func runVersion(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("too many non-flag arguments: %q", args)
	}
	var err error
	var st *ipnstate.Status

	if versionArgs.daemon {
		st, err = localClient.StatusWithoutPeers(ctx)
		if err != nil {
			return err
		}
	}

	var latestVer string
	if versionArgs.withLatest {
		track := "stable"
		if version.IsUnstableBuild() {
			track = "unstable"
		}
		latestVer, err = latestTailscaleVersion(track)
		if err != nil {
			return err
		}
	}

	if versionArgs.json {
		m := version.GetMeta()
		if st != nil {
			m.DaemonLong = st.Version
		}
		out := struct {
			version.Meta
			Latest string `json:"latest,omitempty"`
		}{
			Meta:   m,
			Latest: latestVer,
		}
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "\t")
		return e.Encode(out)
	}

	if st == nil {
		outln(version.String())
		if versionArgs.withLatest {
			printf("  latest: %s\n", latestVer)
		}
		return nil
	}
	printf("Client: %s\n", version.String())
	printf("Daemon: %s\n", st.Version)
	if versionArgs.withLatest {
		printf("Latest: %s\n", latestVer)
	}
	return nil
}
