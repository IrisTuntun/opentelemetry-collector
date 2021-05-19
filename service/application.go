// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package service handles the command-line, configuration, and runs the
// OpenTelemetry Collector.
package service

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configcheck"
	"go.opentelemetry.io/collector/config/configloader"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/config/experimental/configsource"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/internal/collector/telemetry"
	"go.opentelemetry.io/collector/service/internal/builder"
	"go.opentelemetry.io/collector/service/parserprovider"
)

const (
	servicezPath   = "servicez"
	pipelinezPath  = "pipelinez"
	extensionzPath = "extensionz"
)

// State defines Collector's state.
type State int

const (
	Starting State = iota
	Running
	Closing
	Closed
)

// Collector represents a server providing the OpenTelemetry Collector service.
type Collector struct {
	info    component.BuildInfo
	rootCmd *cobra.Command
	logger  *zap.Logger

	service      *service
	stateChannel chan State

	factories component.Factories

	parserProvider parserprovider.ParserProvider

	// stopTestChan is used to terminate the collector server in end to end tests.
	stopTestChan chan struct{}

	// signalsChannel is used to receive termination signals from the OS.
	signalsChannel chan os.Signal

	// asyncErrorChannel is used to signal a fatal error from any component.
	asyncErrorChannel chan error
}

// Parameters holds configuration for creating a new Collector.
type Parameters struct {
	// Factories component factories.
	Factories component.Factories
	// BuildInfo provides collector server start information.
	BuildInfo component.BuildInfo
	// ParserProvider provides the configuration's Parser.
	// If it is not provided a default provider is used. The default provider loads the configuration
	// from a config file define by the --config command line flag and overrides component's configuration
	// properties supplied via --set command line flag.
	ParserProvider parserprovider.ParserProvider
	// LoggingOptions provides a way to change behavior of zap logging.
	LoggingOptions []zap.Option
}

// New creates and returns a new instance of Collector.
func New(params Parameters) (*Collector, error) {
	if err := configcheck.ValidateConfigFromFactories(params.Factories); err != nil {
		return nil, err
	}

	col := &Collector{
		info:         params.BuildInfo,
		factories:    params.Factories,
		stateChannel: make(chan State, Closed+1),
	}

	rootCmd := &cobra.Command{
		Use:     params.BuildInfo.Command,
		Version: params.BuildInfo.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if col.logger, err = newLogger(params.LoggingOptions); err != nil {
				return fmt.Errorf("failed to get logger: %w", err)
			}

			return col.execute(context.Background())
		},
	}

	// TODO: coalesce this code and expose this information to other components.
	flagSet := new(flag.FlagSet)
	addFlagsFns := []func(*flag.FlagSet){
		configtelemetry.Flags,
		parserprovider.Flags,
		telemetry.Flags,
		builder.Flags,
		loggerFlags,
	}
	for _, addFlags := range addFlagsFns {
		addFlags(flagSet)
	}
	rootCmd.Flags().AddGoFlagSet(flagSet)
	col.rootCmd = rootCmd

	parserProvider := params.ParserProvider
	if parserProvider == nil {
		// use default provider.
		parserProvider = parserprovider.Default()
	}
	col.parserProvider = parserProvider

	return col, nil
}

// Run starts the collector according to the command and configuration
// given by the user, and waits for it to complete.
func (col *Collector) Run() error {
	// From this point on do not show usage in case of error.
	col.rootCmd.SilenceUsage = true

	return col.rootCmd.Execute()
}

// GetStateChannel returns state channel of the collector server.
func (col *Collector) GetStateChannel() chan State {
	return col.stateChannel
}

// Command returns Collector's root command.
func (col *Collector) Command() *cobra.Command {
	return col.rootCmd
}

// GetLogger returns logger used by the Collector.
// The logger is initialized after collector server start.
func (col *Collector) GetLogger() *zap.Logger {
	return col.logger
}

// Shutdown shuts down the collector server.
func (col *Collector) Shutdown() {
	// TODO: Implement a proper shutdown with graceful draining of the pipeline.
	// See https://github.com/open-telemetry/opentelemetry-collector/issues/483.
	defer func() {
		if r := recover(); r != nil {
			col.logger.Info("stopTestChan already closed")
		}
	}()
	close(col.stopTestChan)
}

func (col *Collector) setupTelemetry(ballastSizeBytes uint64) error {
	col.logger.Info("Setting up own telemetry...")

	err := applicationTelemetry.init(col.asyncErrorChannel, ballastSizeBytes, col.logger)
	if err != nil {
		return fmt.Errorf("failed to initialize telemetry: %w", err)
	}

	return nil
}

// runAndWaitForShutdownEvent waits for one of the shutdown events that can happen.
func (col *Collector) runAndWaitForShutdownEvent() {
	col.logger.Info("Everything is ready. Begin running and processing data.")

	// plug SIGTERM signal into a channel.
	col.signalsChannel = make(chan os.Signal, 1)
	signal.Notify(col.signalsChannel, os.Interrupt, syscall.SIGTERM)

	// set the channel to stop testing.
	col.stopTestChan = make(chan struct{})
	col.stateChannel <- Running
	select {
	case err := <-col.asyncErrorChannel:
		col.logger.Error("Asynchronous error received, terminating process", zap.Error(err))
	case s := <-col.signalsChannel:
		col.logger.Info("Received signal from OS", zap.String("signal", s.String()))
	case <-col.stopTestChan:
		col.logger.Info("Received stop test request")
	}
	col.stateChannel <- Closing
}

// setupConfigurationComponents loads the config and starts the components. If all the steps succeeds it
// sets the col.service with the service currently running.
func (col *Collector) setupConfigurationComponents(ctx context.Context) error {
	col.logger.Info("Loading configuration...")

	cp, err := col.parserProvider.Get()
	if err != nil {
		return fmt.Errorf("cannot load configuration's parser: %w", err)
	}

	cfg, err := configloader.Load(cp, col.factories)
	if err != nil {
		return fmt.Errorf("cannot load configuration: %w", err)
	}

	if err = cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	col.logger.Info("Applying configuration...")

	service, err := newService(&settings{
		Factories:         col.factories,
		BuildInfo:         col.info,
		Config:            cfg,
		Logger:            col.logger,
		AsyncErrorChannel: col.asyncErrorChannel,
	})
	if err != nil {
		return err
	}

	err = service.Start(ctx)
	if err != nil {
		return err
	}

	col.service = service

	// If provider is watchable start a goroutine watching for updates.
	if watchable, ok := col.parserProvider.(parserprovider.Watchable); ok {
		go func() {
			err := watchable.WatchForUpdate()
			switch {
			// TODO: Move configsource.ErrSessionClosed to providerparser package to avoid depending on configsource.
			case errors.Is(err, configsource.ErrSessionClosed):
				// This is the case of shutdown of the whole collector server, nothing to do.
				col.logger.Info("Config WatchForUpdate closed", zap.Error(err))
				return
			default:
				col.logger.Warn("Config WatchForUpdated exited", zap.Error(err))
				col.reloadService(context.Background())
			}
		}()
	}

	return nil
}

func (col *Collector) execute(ctx context.Context) error {
	col.logger.Info("Starting "+col.info.Command+"...",
		zap.String("Version", col.info.Version),
		zap.Int("NumCPU", runtime.NumCPU()),
	)
	col.stateChannel <- Starting

	// Set memory ballast
	ballast, ballastSizeBytes := col.createMemoryBallast()

	col.asyncErrorChannel = make(chan error)

	// Setup everything.
	err := col.setupTelemetry(ballastSizeBytes)
	if err != nil {
		return err
	}

	err = col.setupConfigurationComponents(ctx)
	if err != nil {
		return err
	}

	// Everything is ready, now run until an event requiring shutdown happens.
	col.runAndWaitForShutdownEvent()

	// Accumulate errors and proceed with shutting down remaining components.
	var errs []error

	// Begin shutdown sequence.
	runtime.KeepAlive(ballast)
	col.logger.Info("Starting shutdown...")

	if closable, ok := col.parserProvider.(parserprovider.Closeable); ok {
		if err := closable.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to close config: %w", err))
		}
	}

	if col.service != nil {
		if err := col.service.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to shutdown service: %w", err))
		}
	}

	if err := applicationTelemetry.shutdown(); err != nil {
		errs = append(errs, fmt.Errorf("failed to shutdown application telemetry: %w", err))
	}

	col.logger.Info("Shutdown complete.")
	col.stateChannel <- Closed
	close(col.stateChannel)

	return consumererror.Combine(errs)
}

func (col *Collector) createMemoryBallast() ([]byte, uint64) {
	ballastSizeMiB := builder.MemBallastSize()
	if ballastSizeMiB > 0 {
		ballastSizeBytes := uint64(ballastSizeMiB) * 1024 * 1024
		ballast := make([]byte, ballastSizeBytes)
		col.logger.Info("Using memory ballast", zap.Int("MiBs", ballastSizeMiB))
		return ballast, ballastSizeBytes
	}
	return nil, 0
}

// reloadService shutdowns the current col.service and setups a new one according
// to the latest configuration. It requires that col.parserProvider and col.factories
// are properly populated to finish successfully.
func (col *Collector) reloadService(ctx context.Context) error {
	if closeable, ok := col.parserProvider.(parserprovider.Closeable); ok {
		if err := closeable.Close(ctx); err != nil {
			return fmt.Errorf("failed close current config provider: %w", err)
		}
	}

	if col.service != nil {
		retiringService := col.service
		col.service = nil
		if err := retiringService.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown the retiring config: %w", err)
		}
	}

	if err := col.setupConfigurationComponents(ctx); err != nil {
		return fmt.Errorf("failed to setup configuration components: %w", err)
	}

	return nil
}
