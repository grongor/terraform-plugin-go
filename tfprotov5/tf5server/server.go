package tf5server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov5"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5/internal/fromproto"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5/internal/tfplugin5"
	"github.com/hashicorp/terraform-plugin-go/tfprotov5/internal/toproto"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-log/tfsdklog"
	tfaddr "github.com/hashicorp/terraform-registry-address"
	testing "github.com/mitchellh/go-testing-interface"
)

const tflogSubsystemName = "proto"

// Global logging keys attached to all requests.
//
// Practitioners or tooling reading logs may be depending on these keys, so be
// conscious of that when changing them.
const (
	// A unique ID for the RPC request
	logKeyRequestID = "tf_req_id"

	// The full address of the provider, such as
	// registry.terraform.io/hashicorp/random
	logKeyProviderAddress = "tf_provider_addr"

	// The RPC being run, such as "ApplyResourceChange"
	logKeyRPC = "tf_rpc"

	// The type of resource being operated on, such as "random_pet"
	logKeyResourceType = "tf_resource_type"

	// The type of data source being operated on, such as "archive_file"
	logKeyDataSourceType = "tf_data_source_type"

	// The protocol version being used, as a string, such as "5"
	logKeyProtocolVersion = "tf_proto_version"
)

const (
	// envTfReattachProviders is the environment variable used by Terraform CLI
	// to directly connect to already running provider processes, such as those
	// being inspected by debugging processes. When connecting to providers in
	// this manner, Terraform CLI disables certain plugin handshake checks and
	// will not stop the provider process.
	envTfReattachProviders = "TF_REATTACH_PROVIDERS"
)

// ServeOpt is an interface for defining options that can be passed to the
// Serve function. Each implementation modifies the ServeConfig being
// generated. A slice of ServeOpts then, cumulatively applied, render a full
// ServeConfig.
type ServeOpt interface {
	ApplyServeOpt(*ServeConfig) error
}

// ServeConfig contains the configured options for how a provider should be
// served.
type ServeConfig struct {
	logger       hclog.Logger
	debugCtx     context.Context
	debugCh      chan *plugin.ReattachConfig
	debugCloseCh chan struct{}

	managedDebug                      bool
	managedDebugReattachConfigTimeout time.Duration
	managedDebugStopSignals           []os.Signal

	disableLogInitStderr bool
	disableLogLocation   bool
	useLoggingSink       testing.T
	envVar               string
}

type serveConfigFunc func(*ServeConfig) error

func (s serveConfigFunc) ApplyServeOpt(in *ServeConfig) error {
	return s(in)
}

// WithDebug returns a ServeOpt that will set the server into debug mode, using
// the passed options to populate the go-plugin ServeTestConfig.
//
// This is an advanced ServeOpt that assumes the caller will fully manage the
// reattach configuration and server lifecycle. Refer to WithManagedDebug for a
// ServeOpt that handles common use cases, such as implementing provider main
// functions.
func WithDebug(ctx context.Context, config chan *plugin.ReattachConfig, closeCh chan struct{}) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		if in.managedDebug {
			return errors.New("cannot set both WithDebug and WithManagedDebug")
		}

		in.debugCtx = ctx
		in.debugCh = config
		in.debugCloseCh = closeCh
		return nil
	})
}

// WithManagedDebug returns a ServeOpt that will start the server in debug
// mode, managing the reattach configuration handling and server lifecycle.
// Reattach configuration is output to stdout with human friendly instructions.
// By default, the server can be stopped with os.Interrupt (SIGINT; ctrl-c).
//
// Refer to the optional WithManagedDebugStopSignals and
// WithManagedDebugReattachConfigTimeout ServeOpt for additional configuration.
//
// The reattach configuration output of this handling is not protected by
// compatibility guarantees. Use the WithDebug ServeOpt for advanced use cases.
func WithManagedDebug() ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		if in.debugCh != nil {
			return errors.New("cannot set both WithDebug and WithManagedDebug")
		}

		in.managedDebug = true
		return nil
	})
}

// WithManagedDebugStopSignals returns a ServeOpt that will set the stop signals for a
// debug managed process (WithManagedDebug). When not configured, os.Interrupt
// (SIGINT; Ctrl-c) will stop the process.
func WithManagedDebugStopSignals(signals []os.Signal) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.managedDebugStopSignals = signals
		return nil
	})
}

// WithManagedDebugReattachConfigTimeout returns a ServeOpt that will set the timeout
// for a debug managed process to start and return its reattach configuration.
// When not configured, 2 seconds is the default.
func WithManagedDebugReattachConfigTimeout(timeout time.Duration) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.managedDebugReattachConfigTimeout = timeout
		return nil
	})
}

// WithGoPluginLogger returns a ServeOpt that will set the logger that
// go-plugin should use to log messages.
func WithGoPluginLogger(logger hclog.Logger) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.logger = logger
		return nil
	})
}

// WithLoggingSink returns a ServeOpt that will enable the logging sink, which
// is used in test frameworks to control where terraform-plugin-log output is
// written and at what levels, mimicking Terraform's logging sink behaviors.
func WithLoggingSink(t testing.T) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.useLoggingSink = t
		return nil
	})
}

// WithoutLogStderrOverride returns a ServeOpt that will disable the
// terraform-plugin-log behavior of logging to the stderr that existed at
// startup, not the stderr that exists when the logging statement is called.
func WithoutLogStderrOverride() ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.disableLogInitStderr = true
		return nil
	})
}

// WithoutLogLocation returns a ServeOpt that will exclude file names and line
// numbers from log output for the terraform-plugin-log logs generated by the
// SDKs and provider.
func WithoutLogLocation() ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		in.disableLogLocation = true
		return nil
	})
}

// WithLogEnvVarName sets the name of the provider for the purposes of the
// logging environment variable that controls the provider's log level. It is
// the part following TF_LOG_PROVIDER_ and defaults to the name part of the
// provider's registry address, or disabled if it can't parse the provider's
// registry address. Name must only contain letters, numbers, and hyphens.
func WithLogEnvVarName(name string) ServeOpt {
	return serveConfigFunc(func(in *ServeConfig) error {
		if !regexp.MustCompile(`^[a-zA-Z0-9-]+$`).MatchString(name) {
			return errors.New("environment variable names can only contain a-z, A-Z, 0-9, and -")
		}
		in.envVar = name
		return nil
	})
}

// Serve starts a tfprotov5.ProviderServer serving, ready for Terraform to
// connect to it. The name passed in should be the fully qualified name that
// users will enter in the source field of the required_providers block, like
// "registry.terraform.io/hashicorp/time".
//
// Zero or more options to configure the server may also be passed. The default
// invocation is sufficient, but if the provider wants to run in debug mode or
// modify the logger that go-plugin is using, ServeOpts can be specified to
// support that.
func Serve(name string, serverFactory func() tfprotov5.ProviderServer, opts ...ServeOpt) error {
	// Defaults
	conf := ServeConfig{
		managedDebugReattachConfigTimeout: 2 * time.Second,
		managedDebugStopSignals:           []os.Signal{os.Interrupt},
	}

	for _, opt := range opts {
		err := opt.ApplyServeOpt(&conf)
		if err != nil {
			return err
		}
	}

	serveConfig := &plugin.ServeConfig{
		HandshakeConfig: plugin.HandshakeConfig{
			ProtocolVersion:  5,
			MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
			MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
		},
		Plugins: plugin.PluginSet{
			"provider": &GRPCProviderPlugin{
				GRPCProvider: serverFactory,
			},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	}

	if conf.logger != nil {
		serveConfig.Logger = conf.logger
	}

	if conf.managedDebug {
		ctx, cancel := context.WithCancel(context.Background())
		signalCh := make(chan os.Signal, len(conf.managedDebugStopSignals))

		signal.Notify(signalCh, conf.managedDebugStopSignals...)

		defer func() {
			signal.Stop(signalCh)
			cancel()
		}()

		go func() {
			select {
			case <-signalCh:
				cancel()
			case <-ctx.Done():
			}
		}()

		conf.debugCh = make(chan *plugin.ReattachConfig)
		conf.debugCloseCh = make(chan struct{})
		conf.debugCtx = ctx
	}

	if conf.debugCh != nil {
		serveConfig.Test = &plugin.ServeTestConfig{
			Context:          conf.debugCtx,
			ReattachConfigCh: conf.debugCh,
			CloseCh:          conf.debugCloseCh,
		}
	}

	if !conf.managedDebug {
		plugin.Serve(serveConfig)
		return nil
	}

	go plugin.Serve(serveConfig)

	var pluginReattachConfig *plugin.ReattachConfig

	select {
	case pluginReattachConfig = <-conf.debugCh:
	case <-time.After(conf.managedDebugReattachConfigTimeout):
		return errors.New("timeout waiting on reattach configuration")
	}

	if pluginReattachConfig == nil {
		return errors.New("nil reattach configuration received")
	}

	// Duplicate implementation is required because the go-plugin
	// ReattachConfig.Addr implementation is not friendly for JSON encoding
	// and to avoid importing terraform-exec.
	type reattachConfigAddr struct {
		Network string
		String  string
	}

	type reattachConfig struct {
		Protocol        string
		ProtocolVersion int
		Pid             int
		Test            bool
		Addr            reattachConfigAddr
	}

	reattachBytes, err := json.Marshal(map[string]reattachConfig{
		name: {
			Protocol:        string(pluginReattachConfig.Protocol),
			ProtocolVersion: pluginReattachConfig.ProtocolVersion,
			Pid:             pluginReattachConfig.Pid,
			Test:            pluginReattachConfig.Test,
			Addr: reattachConfigAddr{
				Network: pluginReattachConfig.Addr.Network(),
				String:  pluginReattachConfig.Addr.String(),
			},
		},
	})

	if err != nil {
		return fmt.Errorf("Error building reattach string: %w", err)
	}

	reattachStr := string(reattachBytes)

	// This is currently intended to be executed via provider main function and
	// human friendly, so output directly to stdout.
	fmt.Printf("Provider started. To attach Terraform CLI, set the %s environment variable with the following:\n\n", envTfReattachProviders)

	switch runtime.GOOS {
	case "windows":
		fmt.Printf("\tCommand Prompt:\tset \"%s=%s\"\n", envTfReattachProviders, reattachStr)
		fmt.Printf("\tPowerShell:\t$env:%s='%s'\n", envTfReattachProviders, strings.ReplaceAll(reattachStr, `'`, `''`))
	default:
		fmt.Printf("\t%s='%s'\n", envTfReattachProviders, strings.ReplaceAll(reattachStr, `'`, `'"'"'`))
	}

	fmt.Println("")

	// Wait for the server to be done.
	<-conf.debugCloseCh

	return nil
}

type server struct {
	downstream tfprotov5.ProviderServer
	tfplugin5.UnimplementedProviderServer

	stopMu sync.Mutex
	stopCh chan struct{}

	tflogSDKOpts tfsdklog.Options
	tflogOpts    tflog.Options
	useTFLogSink bool
	testHandle   testing.T
	name         string
}

func mergeStop(ctx context.Context, cancel context.CancelFunc, stopCh chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-stopCh:
		cancel()
	}
}

// stoppableContext returns a context that wraps `ctx` but will be canceled
// when the server's stopCh is closed.
//
// This is used to cancel all in-flight contexts when the Stop method of the
// server is called.
func (s *server) stoppableContext(ctx context.Context) context.Context {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	stoppable, cancel := context.WithCancel(ctx)
	go mergeStop(stoppable, cancel, s.stopCh)
	return stoppable
}

// loggingContext returns a context that wraps `ctx` and has
// terraform-plugin-log loggers injected.
func (s *server) loggingContext(ctx context.Context) context.Context {
	if s.useTFLogSink {
		ctx = tfsdklog.RegisterTestSink(ctx, s.testHandle)
	}

	// generate a request ID
	reqID, err := uuid.GenerateUUID()
	if err != nil {
		reqID = "unable to assign request ID: " + err.Error()
	}

	// set up the logger SDK loggers are derived from
	ctx = tfsdklog.NewRootSDKLogger(ctx, append(tfsdklog.Options{
		tfsdklog.WithLevelFromEnv("TF_LOG_SDK"),
	}, s.tflogSDKOpts...)...)
	ctx = tfsdklog.With(ctx, logKeyRequestID, reqID)
	ctx = tfsdklog.With(ctx, logKeyProviderAddress, s.name)

	// set up our protocol-level subsystem logger
	ctx = tfsdklog.NewSubsystem(ctx, tflogSubsystemName, append(tfsdklog.Options{
		tfsdklog.WithLevelFromEnv("TF_LOG_SDK_PROTO"),
	}, s.tflogSDKOpts...)...)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyProtocolVersion, "5")

	// set up the provider logger
	ctx = tfsdklog.NewRootProviderLogger(ctx, s.tflogOpts...)
	ctx = tflog.With(ctx, logKeyRequestID, reqID)
	ctx = tflog.With(ctx, logKeyProviderAddress, s.name)
	return ctx
}

func rpcLoggingContext(ctx context.Context, rpc string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyRPC, rpc)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyRPC, rpc)
	ctx = tflog.With(ctx, logKeyRPC, rpc)
	return ctx
}

func resourceLoggingContext(ctx context.Context, resource string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyResourceType, resource)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyResourceType, resource)
	ctx = tflog.With(ctx, logKeyResourceType, resource)
	return ctx
}

func dataSourceLoggingContext(ctx context.Context, dataSource string) context.Context {
	ctx = tfsdklog.With(ctx, logKeyDataSourceType, dataSource)
	ctx = tfsdklog.SubsystemWith(ctx, tflogSubsystemName, logKeyDataSourceType, dataSource)
	ctx = tflog.With(ctx, logKeyDataSourceType, dataSource)
	return ctx
}

// New converts a tfprotov5.ProviderServer into a server capable of handling
// Terraform protocol requests and issuing responses using the gRPC types.
func New(name string, serve tfprotov5.ProviderServer, opts ...ServeOpt) tfplugin5.ProviderServer {
	var conf ServeConfig
	for _, opt := range opts {
		err := opt.ApplyServeOpt(&conf)
		if err != nil {
			// this should never happen, we already executed all
			// this code as part of Serve
			panic(err)
		}
	}
	var sdkOptions tfsdklog.Options
	var options tflog.Options
	if !conf.disableLogInitStderr {
		sdkOptions = append(sdkOptions, tfsdklog.WithStderrFromInit())
		options = append(options, tfsdklog.WithStderrFromInit())
	}
	if conf.disableLogLocation {
		sdkOptions = append(sdkOptions, tfsdklog.WithoutLocation())
		options = append(options, tflog.WithoutLocation())
	}
	envVar := conf.envVar
	if envVar == "" {
		addr, err := tfaddr.ParseRawProviderSourceString(name)
		if err != nil {
			log.Printf("[ERROR] Error parsing provider name %q: %s", name, err)
		} else {
			envVar = addr.Type
		}
	}
	envVar = strings.ReplaceAll(envVar, "-", "_")
	if envVar != "" {
		options = append(options, tfsdklog.WithLogName(envVar), tflog.WithLevelFromEnv("TF_LOG_PROVIDER", envVar))
	}
	return &server{
		downstream:   serve,
		stopCh:       make(chan struct{}),
		tflogOpts:    options,
		tflogSDKOpts: sdkOptions,
		name:         name,
		useTFLogSink: conf.useLoggingSink != nil,
		testHandle:   conf.useLoggingSink,
	}
}

func (s *server) GetSchema(ctx context.Context, req *tfplugin5.GetProviderSchema_Request) (*tfplugin5.GetProviderSchema_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "GetSchema")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.GetProviderSchemaRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.GetProviderSchema(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.GetProviderSchema_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) PrepareProviderConfig(ctx context.Context, req *tfplugin5.PrepareProviderConfig_Request) (*tfplugin5.PrepareProviderConfig_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "PrepareProviderConfig")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.PrepareProviderConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.PrepareProviderConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.PrepareProviderConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) Configure(ctx context.Context, req *tfplugin5.Configure_Request) (*tfplugin5.Configure_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "Configure")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ConfigureProviderRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ConfigureProvider(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.Configure_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

// stop closes the stopCh associated with the server and replaces it with a new
// one.
//
// This causes all in-flight requests for the server to have their contexts
// canceled.
func (s *server) stop() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()

	close(s.stopCh)
	s.stopCh = make(chan struct{})
}

func (s *server) Stop(ctx context.Context, req *tfplugin5.Stop_Request) (*tfplugin5.Stop_Response, error) {
	ctx = rpcLoggingContext(s.loggingContext(ctx), "Stop")
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.StopProviderRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.StopProvider(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Closing all our contexts")
	s.stop()
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Closed all our contexts")
	ret, err := toproto.Stop_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ValidateDataSourceConfig(ctx context.Context, req *tfplugin5.ValidateDataSourceConfig_Request) (*tfplugin5.ValidateDataSourceConfig_Response, error) {
	ctx = dataSourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ValidateDataSourceConfig"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ValidateDataSourceConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ValidateDataSourceConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ValidateDataSourceConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfplugin5.ReadDataSource_Request) (*tfplugin5.ReadDataSource_Response, error) {
	ctx = dataSourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ReadDataSource"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ReadDataSourceRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ReadDataSource(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ReadDataSource_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ValidateResourceTypeConfig(ctx context.Context, req *tfplugin5.ValidateResourceTypeConfig_Request) (*tfplugin5.ValidateResourceTypeConfig_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ValidateResourceTypeConfig"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ValidateResourceTypeConfigRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ValidateResourceTypeConfig(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ValidateResourceTypeConfig_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfplugin5.UpgradeResourceState_Request) (*tfplugin5.UpgradeResourceState_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "UpgradeResourceState"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.UpgradeResourceStateRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.UpgradeResourceState(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.UpgradeResourceState_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ReadResource(ctx context.Context, req *tfplugin5.ReadResource_Request) (*tfplugin5.ReadResource_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ReadResource"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ReadResourceRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ReadResource(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ReadResource_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfplugin5.PlanResourceChange_Request) (*tfplugin5.PlanResourceChange_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "PlanResourceChange"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.PlanResourceChangeRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.PlanResourceChange(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.PlanResourceChange_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfplugin5.ApplyResourceChange_Request) (*tfplugin5.ApplyResourceChange_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ApplyResourceChange"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ApplyResourceChangeRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ApplyResourceChange(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ApplyResourceChange_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}

func (s *server) ImportResourceState(ctx context.Context, req *tfplugin5.ImportResourceState_Request) (*tfplugin5.ImportResourceState_Response, error) {
	ctx = resourceLoggingContext(rpcLoggingContext(s.loggingContext(ctx), "ImportResourceState"), req.TypeName)
	ctx = s.stoppableContext(ctx)
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Received request")
	defer tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Served request")
	r, err := fromproto.ImportResourceStateRequest(req)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting request from protobuf", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Calling downstream")
	resp, err := s.downstream.ImportResourceState(ctx, r)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error from downstream", "error", err)
		return nil, err
	}
	tfsdklog.SubsystemTrace(ctx, tflogSubsystemName, "Called downstream")
	ret, err := toproto.ImportResourceState_Response(resp)
	if err != nil {
		tfsdklog.SubsystemError(ctx, tflogSubsystemName, "Error converting response to protobuf", "error", err)
		return nil, err
	}
	return ret, nil
}
