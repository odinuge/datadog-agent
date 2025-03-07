// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package check

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/DataDog/datadog-agent/cmd/agent/command"
	"github.com/DataDog/datadog-agent/comp/core"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
)

func TestCommand(t *testing.T) {
	fxutil.TestOneShotSubcommand(t,
		Commands(&command.GlobalParams{}),
		// this command has a lot of options, so just test a few
		[]string{"check", "cleopatra", "--delay", "1", "--flare"},
		run,
		func(cliParams *cliParams, coreParams core.BundleParams) {
			require.Equal(t, []string{"cleopatra"}, cliParams.args)
			require.Equal(t, 1, cliParams.checkDelay)
			require.True(t, cliParams.saveFlare)
			require.Equal(t, true, coreParams.ConfigLoadSecrets)
		})
}
