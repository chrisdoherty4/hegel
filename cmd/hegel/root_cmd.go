package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "net/http/pprof" //nolint:gosec // G108: Profiling endpoint is automatically exposed on /debug/pprof

	"github.com/equinix-labs/otel-init-go/otelinit"
	"github.com/oklog/run"
	"github.com/packethost/pkg/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/tinkerbell/hegel/build"
	"github.com/tinkerbell/hegel/datamodel"
	grpcserver "github.com/tinkerbell/hegel/grpc-server"
	"github.com/tinkerbell/hegel/hardware"
	httpserver "github.com/tinkerbell/hegel/http-server"
	"github.com/tinkerbell/hegel/metrics"
)

const longHelp = `
Run a Hegel server.

Each CLI argument has a corresponding environment variable in the form of the CLI argument prefixed with HEGEL. If both
the flag and environment variable form are specified, the flag form takes precedence.

Examples
  --factility              HEGEL_FACILITY
  --http-port              HEGEL_HTTP_PORT
  --http-custom-endpoints  HEGEL_HTTP_CUSTOM_ENDPOINTS

For backwards compatibility a set of deprecated CLI and environment variables are still supported. Behavior for
specifying both deprecated and current forms is undefined.

Deprecated CLI flags
  Deprecated   Current
  --http_port  --http-port
  --use_tls    --grpc-use-tls
  --tls_cert   --grpc-tls-cert
  --tls_key    --grpc-tls-key

Deprecated environment variables
  Deprecated          Current
  DATA_MODEL_VERSION  HEGEL_DATA_MODEL
  GRPC_PORT           HEGEL_GRPC_PORT
  HEGEL_USE_TLS       HEGEL_GRPC_USE_TLS
  HEGEL_TLS_CERT      HEGEL_GRPC_TLS_CERT
  HEGEL_TLS_KEY       HEGEL_GRPC_TLS_KEY
  CUSTOM_ENDPOINTS    HEGEL_HTTP_CUSTOM_ENDPOINTS
  TRUSTED_PROXIES     HEGEL_TRUSTED_PROXIES
`

// EnvNamePrefix defines the environment variable prefix required for all environment configuration.
const EnvNamePrefix = "HEGEL"

// RootCommandOptions encompasses all the configurability of the RootCommand.
type RootCommandOptions struct {
	Facility       string `mapstructure:"facility"`
	DataModel      string `mapstructure:"data-model"`
	TrustedProxies string `mapstructure:"trusted-proxies"`

	HTTPPort            int    `mapstructure:"http-port"`
	HTTPCustomEndpoints string `mapstructure:"http-custom-endpoints"`

	GRPCPort        int    `mapstructure:"grpc-port"`
	GRPCUseTLS      bool   `mapstructure:"grpc-use-tls"`
	GRPCTLSCertPath string `mapstructure:"grpc-tls-cert"`
	GRPCTLSKeyPath  string `mapstructure:"grpc-tls-key"`

	KubeAPI    string `mapstructure:"kubernetes-api"`
	Kubeconfig string `mapstructure:"kubeconfig"`
}

func (o RootCommandOptions) GetDataModel() datamodel.DataModel {
	return datamodel.DataModel(o.DataModel)
}

// RootCommand is the root command that represents the entrypoint to Hegel.
type RootCommand struct {
	*cobra.Command
	vpr  *viper.Viper
	Opts RootCommandOptions
}

// Temporary workaround to circumvent the linter until the root command is wired up.
var _, _ = NewRootCommand()

// NewRootCommand creates new RootCommand instance.
func NewRootCommand() (*RootCommand, error) {
	rootCmd := &RootCommand{
		Command: &cobra.Command{
			Use:  os.Args[0],
			Long: longHelp,
		},
	}

	rootCmd.PreRunE = rootCmd.PreRun
	rootCmd.RunE = rootCmd.Run
	rootCmd.Flags().SortFlags = false // Print flag help in the order they're specified.

	// Ensure keys with `-` use `_` for env keys else Viper won't match them.
	rootCmd.vpr = viper.NewWithOptions(viper.EnvKeyReplacer(strings.NewReplacer("-", "_")))
	rootCmd.vpr.SetEnvPrefix(EnvNamePrefix)

	if err := rootCmd.configureFlags(); err != nil {
		return nil, err
	}

	if err := rootCmd.configureLegacyFlags(); err != nil {
		return nil, err
	}

	return rootCmd, nil
}

// PreRun satisfies cobra.Command.PreRunE and unmarshalls. Its responsible for populating c.Opts.
func (c *RootCommand) PreRun(*cobra.Command, []string) error {
	if err := c.vpr.Unmarshal(&c.Opts); err != nil {
		return err
	}

	return c.validateOpts()
}

// Run executes Hegel.
func (c *RootCommand) Run(cmd *cobra.Command, _ []string) error {
	cmdLogger, err := log.Init("github.com/tinkerbell/hegel")
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	defer cmdLogger.Close()
	metrics.Init(cmdLogger)

	logger := cmdLogger.Package("main")

	logger.With("opts", fmt.Sprintf("%+v", c.Opts)).Info("root command options")

	ctx, otelShutdown := otelinit.InitOpenTelemetry(cmd.Context(), "hegel")
	defer otelShutdown(ctx)

	metrics.State.Set(metrics.Initializing)

	hardwareClient, err := hardware.NewClient(c.Opts.Facility, c.Opts.GetDataModel())
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	grpcServer := grpcserver.NewServer(cmdLogger, hardwareClient)

	ctx, cancel := context.WithCancel(cmd.Context())
	var routines run.Group

	routines.Add(
		func() error {
			return httpserver.Serve(
				ctx,
				logger,
				grpcServer,
				c.Opts.HTTPPort,
				build.GetGitRevision(),
				time.Now(),
				c.Opts.GetDataModel(),
				c.Opts.HTTPCustomEndpoints,
				c.Opts.TrustedProxies,
			)
		},
		func(error) { cancel() },
	)

	routines.Add(
		func() error {
			return grpcserver.Serve(
				ctx,
				logger,
				grpcServer,
				c.Opts.GRPCPort,
				c.Opts.TrustedProxies,
				c.Opts.GRPCTLSCertPath,
				c.Opts.GRPCTLSKeyPath,
				c.Opts.GRPCUseTLS,
			)
		},
		func(error) { cancel() },
	)

	signalsCh := make(chan os.Signal, 1)
	signal.Notify(signalsCh, os.Interrupt, syscall.SIGTERM)
	routines.Add(
		func() error {
			select {
			case sig, ok := <-signalsCh:
				if ok {
					cmdLogger.With("signal", sig).Info("received stop signal, gracefully shutting down")
					return context.Canceled
				}
			case <-ctx.Done():
			}
			return nil
		},
		func(error) { cancel() },
	)

	return routines.Run()
}

func (c *RootCommand) configureFlags() error {
	c.Flags().String("facility", "onprem", "The facility we are running in (mostly to connect to cacher)")
	c.Flags().String("data-model", string(datamodel.TinkServer), "The back-end data source: [\"1\", \"kubernetes\"] (1 indicates tink server)")
	c.Flags().String("trusted-proxies", "", "A commma separated list of allowed peer IPs and/or CIDR blocks to replace with X-Forwarded-For for both gRPC and HTTP endpoints")

	c.Flags().Int("grpc-port", 42115, "Port to listen on for gRPC requests")
	c.Flags().Bool("grpc-use-tls", true, "Toggle for gRPC TLS usage")
	c.Flags().String("grpc-tls-cert", "", "Path of a TLS certificate for the gRPC server")
	c.Flags().String("grpc-tls-key", "", "Path to the private key for the tls_cert")

	c.Flags().Int("http-port", 50061, "Port to listen on for HTTP requests")
	c.Flags().String("http-custom-endpoints", `{"/metadata":".metadata.instance"}`, "JSON encoded object specifying custom endpoint => metadata mappings")

	c.Flags().String("kubernetes-api", "", "URL of the Kubernetes API Server")
	c.Flags().String("kubeconfig", "", "Path to a kubeconfig file")

	if err := c.vpr.BindPFlags(c.Flags()); err != nil {
		return err
	}

	var err error
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		err = c.vpr.BindEnv(f.Name)
	})

	return err
}

func (c *RootCommand) configureLegacyFlags() error {
	c.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "use_tls":
			return pflag.NormalizedName("grpc-use-tls")
		case "tls_cert":
			return pflag.NormalizedName("grpc-tls-cert")
		case "tls_key":
			return pflag.NormalizedName("grpc-tls-key")
		case "http_port":
			return pflag.NormalizedName("http-port")
		default:
			return pflag.NormalizedName(name)
		}
	})

	for key, envName := range map[string]string{
		"data-model":            "DATA_MODEL_VERSION",
		"grpc-use-tls":          "HEGEL_USE_TLS",
		"grpc-tls-cert":         "HEGEL_TLS_CERT",
		"grpc-tls-key":          "HEGEL_TLS_KEY",
		"http-custom-endpoints": "CUSTOM_ENDPOINTS",
		"trusted-proxies":       "TRUSTED_PROXIES",
	} {
		if err := c.vpr.BindEnv(key, envName); err != nil {
			return err
		}
	}

	return nil
}

func (c *RootCommand) validateOpts() error {
	if c.Opts.GRPCUseTLS {
		if c.Opts.GRPCTLSCertPath == "" {
			return errors.New("--grpc-use-tls requires --grpc-tls-cert")
		}

		if c.Opts.GRPCTLSKeyPath == "" {
			return errors.New("--grpc-use-tls requires --grpc-tls-key")
		}
	}
	if c.Opts.GetDataModel() == datamodel.Kubernetes {
		if c.Opts.Kubeconfig == "" {
			return fmt.Errorf("--data-model=%v requires --kubeconfig", datamodel.Kubernetes)
		}
	}

	return nil
}
