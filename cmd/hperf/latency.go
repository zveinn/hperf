// Copyright (c) 2015-2024 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"github.com/minio/cli"
	"github.com/minio/hperf/client"
	"github.com/minio/hperf/shared"
)

var latencyCMD = cli.Command{
	Name:   "latency",
	Usage:  "start a test to measure latency at the application level",
	Action: runLatency,
	Flags: []cli.Flag{
		hostsFlag,
		portFlag,
		concurrencyFlag,
		delayFlag,
		durationFlag,
		bufferSizeFlag,
		payloadSizeFlag,
		restartOnErrorFlag,
		testIDFlag,
		saveTestFlag,
		dnsServerFlag,
	},
	CustomHelpTemplate: `NAME:
  {{.HelpName}} - {{.Usage}}

USAGE:
  {{.HelpName}} [FLAGS]

FLAGS:
  {{range .VisibleFlags}}{{.}}
  {{end}}
EXAMPLES:
  1. Run a basic test:
   {{.Prompt}} {{.HelpName}} --hosts 10.10.10.1,10.10.10.2

  2. Run a slow moving test to probe latency:
   {{.Prompt}} {{.HelpName}} --hosts 10.10.10.1,10.10.10.2 --request-delay 100 --concurrency 1
`,
}

func runLatency(ctx *cli.Context) error {
	config, err := parseConfig(ctx)
	if err != nil {
		return err
	}
	config.TestType = shared.LatencyTest
	return client.RunTest(GlobalContext, *config)
}
