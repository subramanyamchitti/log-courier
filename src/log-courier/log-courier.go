/*
 * Copyright 2014 Jason Woods.
 *
 * This file is a modification of code from Logstash Forwarder.
 * Copyright 2012-2013 Jordan Sissel and contributors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
  "flag"
  "fmt"
  "github.com/op/go-logging"
  "lc-lib/core"
  "lc-lib/prospector"
  "lc-lib/spooler"
  "lc-lib/publisher"
  "lc-lib/registrar"
  stdlog "log"
  "os"
  "runtime/pprof"
  "time"
)

import _ "lc-lib/codecs"
import _ "lc-lib/transports"

const Log_Courier_Version string = "0.12"

var log *logging.Logger

func init() {
  log = logging.MustGetLogger("")
}

func main() {
  logcourier := NewLogCourier()
  logcourier.Run()
}

type LogCourier struct {
  pipeline       *core.Pipeline
  config         *core.Config
  shutdown_chan  chan os.Signal
  reload_chan    chan os.Signal
  config_file    string
  from_beginning bool
  log_file       *os.File
}

func NewLogCourier() *LogCourier {
  ret := &LogCourier{
    pipeline: core.NewPipeline(),
  }
  return ret
}

func (lc *LogCourier) Run() {
  lc.startUp()

  event_chan := make(chan *core.EventDescriptor, 16)
  publisher_chan := make(chan []*core.EventDescriptor, 1)

  log.Info("Starting pipeline")

  registrar := registrar.NewRegistrar(lc.config.General.PersistDir)
  lc.pipeline.Register(&registrar.PipelineSegment)

  publisher, err := publisher.NewPublisher(&lc.config.Network, registrar)
  if err != nil {
    log.Fatalf("Failed to initialise: %s", err)
  }
  lc.pipeline.Register(&publisher.PipelineSegment)
  lc.pipeline.RegisterConfigReceiver(&publisher.PipelineConfigReceiver)

  spooler := spooler.NewSpooler(&lc.config.General)
  lc.pipeline.Register(&spooler.PipelineSegment)
  lc.pipeline.RegisterConfigReceiver(&spooler.PipelineConfigReceiver)

  prospector, err := prospector.NewProspector(lc.config, lc.from_beginning, registrar)
  if err != nil {
    log.Fatalf("Failed to initialise: %s", err)
  }
  lc.pipeline.Register(&prospector.PipelineSegment)
  lc.pipeline.RegisterConfigReceiver(&prospector.PipelineConfigReceiver)

  // Start the pipeline
  go prospector.Prospect(event_chan)

  go spooler.Spool(event_chan, publisher_chan)

  go publisher.Publish(publisher_chan)

  go registrar.Register()

  log.Notice("Pipeline ready")

  lc.shutdown_chan = make(chan os.Signal, 1)
  lc.reload_chan = make(chan os.Signal, 1)
  lc.registerSignals()

SignalLoop:
  for {
    select {
    case <-lc.shutdown_chan:
      lc.cleanShutdown()
      break SignalLoop
    case <-lc.reload_chan:
      lc.reloadConfig()
    }
  }

  log.Notice("Exiting")

  if lc.log_file != nil {
    lc.log_file.Close()
  }
}

func (lc *LogCourier) startUp() {
  var version bool
  var config_test bool
  var list_supported bool
  var cpu_profile string

  flag.BoolVar(&version, "version", false, "show version information")
  flag.BoolVar(&config_test, "config-test", false, "Test the configuration specified by -config and exit")
  flag.BoolVar(&list_supported, "list-supported", false, "List supported transports and codecs")
  flag.StringVar(&cpu_profile, "cpuprofile", "", "write cpu profile to file")

  flag.StringVar(&lc.config_file, "config", "", "The config file to load")
  flag.BoolVar(&lc.from_beginning, "from-beginning", false, "On first run, read new files from the beginning instead of the end")

  flag.Parse()

  if version {
    fmt.Printf("Log Courier version %s\n", Log_Courier_Version)
    os.Exit(0)
  }

  if list_supported {
    fmt.Printf("Available transports:\n")
    for _, transport := range core.AvailableTransports() {
      fmt.Printf("  %s\n", transport)
    }

    fmt.Printf("Available codecs:\n")
    for _, codec := range core.AvailableCodecs() {
      fmt.Printf("  %s\n", codec)
    }
    os.Exit(0)
  }

  if lc.config_file == "" {
    fmt.Fprintf(os.Stderr, "Please specify a configuration file with -config.\n\n")
    flag.PrintDefaults()
    os.Exit(1)
  }

  err := lc.loadConfig()

  if config_test {
    if err == nil {
      fmt.Printf("Configuration OK\n")
      os.Exit(0)
    }
    fmt.Printf("Configuration test failed: %s\n", err)
    os.Exit(1)
  }

  if err != nil {
    fmt.Printf("Configuration error: %s\n", err)
    os.Exit(1)
  }

  if err = lc.configureLogging(); err != nil {
    fmt.Printf("Failed to initialise logging: %s", err)
    os.Exit(1)
  }

  if cpu_profile != "" {
    log.Notice("Starting CPU profiler")
    f, err := os.Create(cpu_profile)
    if err != nil {
      log.Fatal(err)
    }
    pprof.StartCPUProfile(f)
    go func() {
      time.Sleep(60 * time.Second)
      pprof.StopCPUProfile()
      log.Panic("CPU profile completed")
    }()
  }
}

func (lc *LogCourier) configureLogging() (err error) {
  backends := make([]logging.Backend, 0, 1)

  // First, the stdout backend
  if lc.config.General.LogStdout {
    backends = append(backends, logging.NewLogBackend(os.Stdout, "", stdlog.LstdFlags|stdlog.Lmicroseconds))
  }

  // Log file?
  if lc.config.General.LogFile != "" {
    lc.log_file, err = os.OpenFile(lc.config.General.LogFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0640)
    if err != nil {
      return
    }

    backends = append(backends, logging.NewLogBackend(lc.log_file, "", stdlog.LstdFlags|stdlog.Lmicroseconds))
  }

  if err = lc.configureLoggingPlatform(&backends); err != nil {
    return
  }

  // Set backends BEFORE log level (or we reset log level)
  logging.SetBackend(backends...)

  // Set the logging level
  logging.SetLevel(lc.config.General.LogLevel, "")

  return nil
}

func (lc *LogCourier) loadConfig() error {
  lc.config = core.NewConfig()
  if err := lc.config.Load(lc.config_file); err != nil {
    return err
  }

  if len(lc.config.Files) == 0 {
    return fmt.Errorf("No file groups were found in the configuration.")
  }

  return nil
}

func (lc *LogCourier) reloadConfig() {
  if err := lc.loadConfig(); err != nil {
    log.Warning("Configuration error, reload unsuccessful: %s", err)
    return
  }

  log.Notice("Configuration reload successful")

  // Update the log level
  logging.SetLevel(lc.config.General.LogLevel, "")

  // Pass the new config to the pipeline workers
  lc.pipeline.SendConfig(lc.config)
}

func (lc *LogCourier) cleanShutdown() {
  log.Notice("Initiating shutdown")
  lc.pipeline.Shutdown()
  lc.pipeline.Wait()
}
