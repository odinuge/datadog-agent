// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/DataDog/datadog-agent/cmd/agent/common"
	"github.com/DataDog/datadog-agent/cmd/internal/standalone"
	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/autodiscovery"
	"github.com/DataDog/datadog-agent/pkg/autodiscovery/integration"
	"github.com/DataDog/datadog-agent/pkg/collector"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/metadata/inventories"
	"github.com/DataDog/datadog-agent/pkg/status"
	"github.com/DataDog/datadog-agent/pkg/util/flavor"
	"github.com/DataDog/datadog-agent/pkg/util/hostname"
	"github.com/DataDog/datadog-agent/pkg/util/scrubber"
)

var (
	checkRate                 bool
	checkTimes                int
	checkPause                int
	checkName                 string
	checkDelay                int
	logLevel                  string
	formatJSON                bool
	formatTable               bool
	breakPoint                string
	fullSketches              bool
	saveFlare                 bool
	profileMemory             bool
	profileMemoryDir          string
	profileMemoryFrames       string
	profileMemoryGC           string
	profileMemoryCombine      string
	profileMemorySort         string
	profileMemoryLimit        string
	profileMemoryDiff         string
	profileMemoryFilters      string
	profileMemoryUnit         string
	profileMemoryVerbose      string
	discoveryTimeout          uint
	discoveryRetryInterval    uint
	discoveryMinInstances     uint
	generateIntegrationTraces bool
)

func setupCmd(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&checkRate, "check-rate", "r", false, "check rates by running the check twice with a 1sec-pause between the 2 runs")
	cmd.Flags().IntVarP(&checkTimes, "check-times", "t", 1, "number of times to run the check")
	cmd.Flags().IntVar(&checkPause, "pause", 0, "pause between multiple runs of the check, in milliseconds")
	cmd.Flags().StringVarP(&logLevel, "log-level", "l", "", "set the log level (default 'off') (deprecated, use the env var DD_LOG_LEVEL instead)")
	cmd.Flags().IntVarP(&checkDelay, "delay", "d", 100, "delay between running the check and grabbing the metrics in milliseconds")
	cmd.Flags().BoolVarP(&formatJSON, "json", "", false, "format aggregator and check runner output as json")
	cmd.Flags().BoolVarP(&formatTable, "table", "", false, "format aggregator and check runner output as an ascii table")
	cmd.Flags().StringVarP(&breakPoint, "breakpoint", "b", "", "set a breakpoint at a particular line number (Python checks only)")
	cmd.Flags().BoolVarP(&profileMemory, "profile-memory", "m", false, "run the memory profiler (Python checks only)")
	cmd.Flags().BoolVar(&fullSketches, "full-sketches", false, "output sketches with bins information")
	cmd.Flags().BoolVarP(&saveFlare, "flare", "", false, "save check results to the log dir so it may be reported in a flare")
	cmd.Flags().UintVarP(&discoveryTimeout, "discovery-timeout", "", 5, "max retry duration until Autodiscovery resolves the check template (in seconds)")
	cmd.Flags().UintVarP(&discoveryRetryInterval, "discovery-retry-interval", "", 1, "(unused)")
	cmd.Flags().UintVarP(&discoveryMinInstances, "discovery-min-instances", "", 1, "minimum number of config instances to be discovered before running the check(s)")
	config.Datadog.BindPFlag("cmd.check.fullsketches", cmd.Flags().Lookup("full-sketches")) //nolint:errcheck

	// Power user flags - mark as hidden
	createHiddenStringFlag(cmd, &profileMemoryDir, "m-dir", "", "an existing directory in which to store memory profiling data, ignoring clean-up")
	createHiddenStringFlag(cmd, &profileMemoryFrames, "m-frames", "", "the number of stack frames to consider")
	createHiddenStringFlag(cmd, &profileMemoryGC, "m-gc", "", "whether or not to run the garbage collector to remove noise")
	createHiddenStringFlag(cmd, &profileMemoryCombine, "m-combine", "", "whether or not to aggregate over all traceback frames")
	createHiddenStringFlag(cmd, &profileMemorySort, "m-sort", "", "what to sort by between: lineno | filename | traceback")
	createHiddenStringFlag(cmd, &profileMemoryLimit, "m-limit", "", "the maximum number of sorted results to show")
	createHiddenStringFlag(cmd, &profileMemoryDiff, "m-diff", "", "how to order diff results between: absolute | positive")
	createHiddenStringFlag(cmd, &profileMemoryFilters, "m-filters", "", "comma-separated list of file path glob patterns to filter by")
	createHiddenStringFlag(cmd, &profileMemoryUnit, "m-unit", "", "the binary unit to represent memory usage (kib, mb, etc.). the default is dynamic")
	createHiddenStringFlag(cmd, &profileMemoryVerbose, "m-verbose", "", "whether or not to include potentially noisy sources")
	createHiddenBooleanFlag(cmd, &generateIntegrationTraces, "m-trace", false, "send the integration traces")

	cmd.SetArgs([]string{"checkName"})
}

// Check returns a cobra command to run checks
func Check(loggerName config.LoggerName, confFilePath *string, flagNoColor *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check <check_name>",
		Short: "Run the specified check",
		Long:  `Use this to run a specific check with a specific rate`,
		RunE: func(cmd *cobra.Command, args []string) error {
			configName := ""
			if flavor.GetFlavor() == flavor.ClusterAgent {
				// we'll search for a config file named `datadog-cluster.yaml`
				configName = "datadog-cluster"
			}
			resolvedLogLevel, warnings, err := setupCLI(loggerName, *confFilePath, configName, "", logLevel, "off")
			if err != nil {
				fmt.Printf("Cannot initialize command: %v\n", err)
				return err
			}

			if *flagNoColor {
				color.NoColor = true
			}

			previousIntegrationTracing := false
			if generateIntegrationTraces {
				if config.Datadog.IsSet("integration_tracing") {
					previousIntegrationTracing = config.Datadog.GetBool("integration_tracing")
				}
				config.Datadog.Set("integration_tracing", true)
			}

			if len(args) != 0 {
				checkName = args[0]
			} else {
				cmd.Help() //nolint:errcheck
				return nil
			}

			hostnameDetected, err := hostname.Get(context.TODO())
			if err != nil {
				fmt.Printf("Cannot get hostname, exiting: %v\n", err)
				return err
			}

			// Initializing the aggregator with a flush interval of 0 (to disable the flush goroutines)
			opts := aggregator.DefaultAgentDemultiplexerOptions(nil)
			opts.FlushInterval = 0
			opts.UseNoopForwarder = true
			opts.UseNoopEventPlatformForwarder = true
			opts.UseNoopOrchestratorForwarder = true
			demux := aggregator.InitAndStartAgentDemultiplexer(opts, hostnameDetected)

			common.LoadComponents(context.Background(), config.Datadog.GetString("confd_path"))
			common.AC.LoadAndRun(context.Background())

			// Create the CheckScheduler, but do not attach it to
			// AutoDiscovery.  NOTE: we do not start common.Coll, either.
			collector.InitCheckScheduler(common.Coll)

			waitCtx, cancelTimeout := context.WithTimeout(
				context.Background(), time.Duration(discoveryTimeout)*time.Second)
			allConfigs := common.WaitForConfigsFromAD(waitCtx, []string{checkName}, int(discoveryMinInstances))
			cancelTimeout()

			// make sure the checks in cs are not JMX checks
			for idx := range allConfigs {
				conf := &allConfigs[idx]
				if conf.Name != checkName {
					continue
				}

				if check.IsJMXConfig(*conf) {
					// we'll mimic the check command behavior with JMXFetch by running
					// it with the JSON reporter and the list_with_metrics command.
					fmt.Println("Please consider using the 'jmx' command instead of 'check jmx'")
					selectedChecks := []string{checkName}
					if checkRate {
						if err := standalone.ExecJmxListWithRateMetricsJSON(selectedChecks, resolvedLogLevel, allConfigs); err != nil {
							return fmt.Errorf("while running the jmx check: %v", err)
						}
					} else {
						if err := standalone.ExecJmxListWithMetricsJSON(selectedChecks, resolvedLogLevel, allConfigs); err != nil {
							return fmt.Errorf("while running the jmx check: %v", err)
						}
					}

					instances := []integration.Data{}

					// Retain only non-JMX instances for later
					for _, instance := range conf.Instances {
						if check.IsJMXInstance(conf.Name, instance, conf.InitConfig) {
							continue
						}
						instances = append(instances, instance)
					}

					if len(instances) == 0 {
						fmt.Printf("All instances of '%s' are JMXFetch instances, and have completed running\n", checkName)
						return nil
					}

					conf.Instances = instances
				}
			}

			if profileMemory {
				// If no directory is specified, make a temporary one
				if profileMemoryDir == "" {
					profileMemoryDir, err = ioutil.TempDir("", "datadog-agent-memory-profiler")
					if err != nil {
						return err
					}

					defer func() {
						cleanupErr := os.RemoveAll(profileMemoryDir)
						if cleanupErr != nil {
							fmt.Printf("%s\n", cleanupErr)
						}
					}()
				}

				for idx := range allConfigs {
					conf := &allConfigs[idx]
					if conf.Name != checkName {
						continue
					}

					var data map[string]interface{}

					err = yaml.Unmarshal(conf.InitConfig, &data)
					if err != nil {
						return err
					}

					if data == nil {
						data = make(map[string]interface{})
					}

					data["profile_memory"] = profileMemoryDir
					err = populateMemoryProfileConfig(data)
					if err != nil {
						return err
					}

					y, _ := yaml.Marshal(data)
					conf.InitConfig = y

					break
				}
			} else if breakPoint != "" {
				breakPointLine, err := strconv.Atoi(breakPoint)
				if err != nil {
					fmt.Printf("breakpoint must be an integer\n")
					return err
				}

				for idx := range allConfigs {
					conf := &allConfigs[idx]
					if conf.Name != checkName {
						continue
					}

					var data map[string]interface{}

					err = yaml.Unmarshal(conf.InitConfig, &data)
					if err != nil {
						return err
					}

					if data == nil {
						data = make(map[string]interface{})
					}

					data["set_breakpoint"] = breakPointLine

					y, _ := yaml.Marshal(data)
					conf.InitConfig = y

					break
				}
			}

			cs := collector.GetChecksByNameForConfigs(checkName, allConfigs)

			// something happened while getting the check(s), display some info.
			if len(cs) == 0 {
				for check, error := range autodiscovery.GetConfigErrors() {
					if checkName == check {
						fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: invalid config for %s: %s", color.RedString("Error"), color.YellowString(check), error))
					}
				}
				for check, errors := range collector.GetLoaderErrors() {
					if checkName == check {
						fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: could not load %s:", color.RedString("Error"), color.YellowString(checkName)))
						for loader, error := range errors {
							fmt.Fprintln(color.Output, fmt.Sprintf("* %s: %s", color.YellowString(loader), error))
						}
					}
				}
				for check, warnings := range autodiscovery.GetResolveWarnings() {
					if checkName == check {
						fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: could not resolve %s config:", color.YellowString("Warning"), color.YellowString(check)))
						for _, warning := range warnings {
							fmt.Fprintln(color.Output, fmt.Sprintf("* %s", warning))
						}
					}
				}
				return fmt.Errorf("no valid check found")
			}

			if len(cs) > 1 {
				fmt.Println("Multiple check instances found, running each of them")
			}

			var checkFileOutput bytes.Buffer
			var instancesData []interface{}
			printer := aggregator.AgentDemultiplexerPrinter{AgentDemultiplexer: demux}
			for _, c := range cs {
				s := runCheck(c, printer)

				// Sleep for a while to allow the aggregator to finish ingesting all the metrics/events/sc
				time.Sleep(time.Duration(checkDelay) * time.Millisecond)

				if formatJSON {
					aggregatorData := printer.GetMetricsDataForPrint()
					var collectorData map[string]interface{}

					collectorJSON, _ := status.GetCheckStatusJSON(c, s)
					err = json.Unmarshal(collectorJSON, &collectorData)
					if err != nil {
						return err
					}

					checkRuns := collectorData["runnerStats"].(map[string]interface{})["Checks"].(map[string]interface{})[checkName].(map[string]interface{})

					// There is only one checkID per run so we'll just access that
					var runnerData map[string]interface{}
					for _, checkIDData := range checkRuns {
						runnerData = checkIDData.(map[string]interface{})
						break
					}

					instanceData := map[string]interface{}{
						"aggregator":  aggregatorData,
						"runner":      runnerData,
						"inventories": collectorData["inventories"],
					}
					instancesData = append(instancesData, instanceData)
				} else if profileMemory {
					// Every instance will create its own directory
					instanceID := strings.SplitN(string(c.ID()), ":", 2)[1]
					// Colons can't be part of Windows file paths
					instanceID = strings.Replace(instanceID, ":", "_", -1)
					profileDataDir := filepath.Join(profileMemoryDir, checkName, instanceID)

					snapshotDir := filepath.Join(profileDataDir, "snapshots")
					if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
						snapshots, err := ioutil.ReadDir(snapshotDir)
						if err != nil {
							return err
						}

						numSnapshots := len(snapshots)
						if numSnapshots > 0 {
							lastSnapshot := snapshots[numSnapshots-1]
							snapshotContents, err := ioutil.ReadFile(filepath.Join(snapshotDir, lastSnapshot.Name()))
							if err != nil {
								return err
							}

							color.HiWhite(string(snapshotContents))
						} else {
							return fmt.Errorf("no snapshots found in %s", snapshotDir)
						}
					} else {
						return fmt.Errorf("no snapshot data found in %s", profileDataDir)
					}

					diffDir := filepath.Join(profileDataDir, "diffs")
					if _, err := os.Stat(diffDir); !os.IsNotExist(err) {
						diffs, err := ioutil.ReadDir(diffDir)
						if err != nil {
							return err
						}

						numDiffs := len(diffs)
						if numDiffs > 0 {
							lastDiff := diffs[numDiffs-1]
							diffContents, err := ioutil.ReadFile(filepath.Join(diffDir, lastDiff.Name()))
							if err != nil {
								return err
							}

							color.HiCyan(fmt.Sprintf("\n%s\n\n", strings.Repeat("=", 50)))
							color.HiWhite(string(diffContents))
						} else {
							return fmt.Errorf("no diffs found in %s", diffDir)
						}
					} else if !singleCheckRun() {
						return fmt.Errorf("no diff data found in %s", profileDataDir)
					}
				} else {
					printer.PrintMetrics(&checkFileOutput, formatTable)

					p := func(data string) {
						fmt.Println(data)
						checkFileOutput.WriteString(data + "\n")
					}

					checkStatus, _ := status.GetCheckStatus(c, s)
					p(string(checkStatus))

					metadata := inventories.GetCheckMetadata(c)
					if metadata != nil {
						p("  Metadata\n  ========")
						for k, v := range *metadata {
							p(fmt.Sprintf("    %s: %v", k, v))
						}
					}
				}
			}

			if runtime.GOOS == "windows" {
				standalone.PrintWindowsUserWarning("check")
			}

			if formatJSON {
				instancesJSON, _ := json.MarshalIndent(instancesData, "", "  ")
				instanceJSONString := string(instancesJSON)

				fmt.Println(instanceJSONString)
				checkFileOutput.WriteString(instanceJSONString + "\n")
			} else if singleCheckRun() {
				if profileMemory {
					color.Yellow("Check has run only once, to collect diff data run the check multiple times with the -t/--check-times flag.")
				} else {
					color.Yellow("Check has run only once, if some metrics are missing you can try again with --check-rate to see any other metric if available.")
				}
			}

			if warnings != nil && warnings.TraceMallocEnabledWithPy2 {
				return errors.New("tracemalloc is enabled but unavailable with python version 2")
			}

			if saveFlare {
				writeCheckToFile(checkName, &checkFileOutput)
			}

			if generateIntegrationTraces {
				config.Datadog.Set("integration_tracing", previousIntegrationTracing)
			}

			return nil
		},
	}
	setupCmd(cmd)
	return cmd
}

func runCheck(c check.Check, demux aggregator.Demultiplexer) *check.Stats {
	s := check.NewStats(c)
	times := checkTimes
	pause := checkPause
	if checkRate {
		if checkTimes > 2 {
			color.Yellow("The check-rate option is overriding check-times to 2")
		}
		if pause > 0 {
			color.Yellow("The check-rate option is overriding pause to 1000ms")
		}
		times = 2
		pause = 1000
	}
	for i := 0; i < times; i++ {
		t0 := time.Now()
		err := c.Run()
		warnings := c.GetWarnings()
		sStats, _ := c.GetSenderStats()
		s.Add(time.Since(t0), err, warnings, sStats)
		if pause > 0 && i < times-1 {
			time.Sleep(time.Duration(pause) * time.Millisecond)
		}
	}

	return s
}

func writeCheckToFile(checkName string, checkFileOutput *bytes.Buffer) {
	_ = os.Mkdir(common.DefaultCheckFlareDirectory, os.ModeDir)

	// Windows cannot accept ":" in file names
	filenameSafeTimeStamp := strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339), ":", "-")
	flarePath := filepath.Join(common.DefaultCheckFlareDirectory, "check_"+checkName+"_"+filenameSafeTimeStamp+".log")

	scrubbed, err := scrubber.ScrubBytes(checkFileOutput.Bytes())
	if err != nil {
		fmt.Println("Error while scrubbing the check file:", err)
	}
	err = ioutil.WriteFile(flarePath, scrubbed, os.ModePerm)

	if err != nil {
		fmt.Println("Error while writing the check file (is the location writable by the dd-agent user?):", err)
	} else {
		fmt.Println("check written to:", flarePath)
	}
}

func singleCheckRun() bool {
	return checkRate == false && checkTimes < 2
}

func createHiddenStringFlag(cmd *cobra.Command, p *string, name string, value string, usage string) {
	cmd.Flags().StringVar(p, name, value, usage)
	cmd.Flags().MarkHidden(name) //nolint:errcheck
}

func createHiddenBooleanFlag(cmd *cobra.Command, p *bool, name string, value bool, usage string) {
	cmd.Flags().BoolVar(p, name, value, usage)
	cmd.Flags().MarkHidden(name) //nolint:errcheck
}

func populateMemoryProfileConfig(initConfig map[string]interface{}) error {
	if profileMemoryFrames != "" {
		profileMemoryFrames, err := strconv.Atoi(profileMemoryFrames)
		if err != nil {
			return fmt.Errorf("--m-frames must be an integer")
		}
		initConfig["profile_memory_frames"] = profileMemoryFrames
	}

	if profileMemoryGC != "" {
		profileMemoryGC, err := strconv.Atoi(profileMemoryGC)
		if err != nil {
			return fmt.Errorf("--m-gc must be an integer")
		}

		initConfig["profile_memory_gc"] = profileMemoryGC
	}

	if profileMemoryCombine != "" {
		profileMemoryCombine, err := strconv.Atoi(profileMemoryCombine)
		if err != nil {
			return fmt.Errorf("--m-combine must be an integer")
		}

		if profileMemoryCombine != 0 && profileMemorySort == "traceback" {
			return fmt.Errorf("--m-combine cannot be sorted (--m-sort) by traceback")
		}

		initConfig["profile_memory_combine"] = profileMemoryCombine
	}

	if profileMemorySort != "" {
		if profileMemorySort != "lineno" && profileMemorySort != "filename" && profileMemorySort != "traceback" {
			return fmt.Errorf("--m-sort must one of: lineno | filename | traceback")
		}
		initConfig["profile_memory_sort"] = profileMemorySort
	}

	if profileMemoryLimit != "" {
		profileMemoryLimit, err := strconv.Atoi(profileMemoryLimit)
		if err != nil {
			return fmt.Errorf("--m-limit must be an integer")
		}
		initConfig["profile_memory_limit"] = profileMemoryLimit
	}

	if profileMemoryDiff != "" {
		if profileMemoryDiff != "absolute" && profileMemoryDiff != "positive" {
			return fmt.Errorf("--m-diff must one of: absolute | positive")
		}
		initConfig["profile_memory_diff"] = profileMemoryDiff
	}

	if profileMemoryFilters != "" {
		initConfig["profile_memory_filters"] = profileMemoryFilters
	}

	if profileMemoryUnit != "" {
		initConfig["profile_memory_unit"] = profileMemoryUnit
	}

	if profileMemoryVerbose != "" {
		profileMemoryVerbose, err := strconv.Atoi(profileMemoryVerbose)
		if err != nil {
			return fmt.Errorf("--m-verbose must be an integer")
		}
		initConfig["profile_memory_verbose"] = profileMemoryVerbose
	}

	return nil
}
