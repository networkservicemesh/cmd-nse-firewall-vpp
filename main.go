// Copyright (c) 2021 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux

package main

import (
	"context"
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/clienturl"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/connect"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/replacelabels"
	"github.com/networkservicemesh/sdk/pkg/tools/token"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/govpp/binapi/acl_types"
	"github.com/edwarnicke/grpcfd"
	"github.com/edwarnicke/vpphelper"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/yaml.v2"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	registryapi "github.com/networkservicemesh/api/pkg/api/registry"

	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/acl"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/mechanisms/memif"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/up"
	"github.com/networkservicemesh/sdk-vpp/pkg/networkservice/xconnect"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanismtranslation"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"
	"github.com/networkservicemesh/sdk/pkg/networkservice/utils/metadata"
	registryclient "github.com/networkservicemesh/sdk/pkg/registry/chains/client"
	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/jaeger"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/opentracing"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
)

// Config holds configuration parameters from environment variables
type Config struct {
	Name             string              `default:"firewall-server" desc:"Name of Firewall Server"`
	ListenOn         string              `default:"listen.on.sock" desc:"listen on socket" split_words:"true"`
	ConnectTo        url.URL             `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime time.Duration       `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	ServiceName      string              `default:"" desc:"Name of providing service" split_words:"true"`
	Labels           map[string]string   `default:"" desc:"Endpoint labels"`
	ACLConfigPath    string              `default:"/etc/vppagent-firewall/config.yaml" desc:"Path to ACL config file" split_words:"true"`
	ACLConfig        []acl_types.ACLRule `default:"" desc:"configured acl rules"`
	LogLevel         string              `default:"INFO" desc:"Log level" split_words:"true"`
}

// Process prints and processes env to config
func (c *Config) Process() error {
	if err := envconfig.Usage("nsm", c); err != nil {
		return errors.Wrap(err, "cannot show usage of envconfig nsm")
	}
	if err := envconfig.Process("nsm", c); err != nil {
		return errors.Wrap(err, "cannot process envconfig nsm")
	}
	return nil
}

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx, cancel := notifyContext()
	defer cancel()
	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))
	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}
	// ********************************************************************************
	// Configure open tracing
	// ********************************************************************************
	log.EnableTracing(true)
	jaegerCloser := jaeger.InitJaeger(ctx, "cmd-nse-firewall-vpp")
	defer func() { _ = jaegerCloser.Close() }()
	// enumerating phases
	log.FromContext(ctx).Infof("there are 6 phases which will be executed followed by a success message:")
	log.FromContext(ctx).Infof("the phases include:")
	log.FromContext(ctx).Infof("1: get config from environment")
	log.FromContext(ctx).Infof("2: retrieve spiffe svid")
	log.FromContext(ctx).Infof("3: create grpc client options")
	log.FromContext(ctx).Infof("4: create firewall network service endpoint")
	log.FromContext(ctx).Infof("5: create grpc and mount nse")
	log.FromContext(ctx).Infof("6: register nse with nsm")
	log.FromContext(ctx).Infof("a final success message with start time duration")

	starttime := time.Now()

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 1: get config from environment")
	// ********************************************************************************
	config := new(Config)
	if err := config.Process(); err != nil {
		logrus.Fatal(err.Error())
	}
	l, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		logrus.Fatalf("invalid log level %s", config.LogLevel)
	}
	logrus.SetLevel(l)

	config.retrieveACLRules(ctx)

	log.FromContext(ctx).Infof("Config: %#v", config)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 2: retrieving svid, check spire agent logs if this is the last line you see")
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	log.FromContext(ctx).Infof("SVID: %q", svid.ID)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 3: create grpc client options")
	// ********************************************************************************
	clientOptions := append(
		opentracing.WithTracingDial(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime))),
		),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 4: create firewall network service endpoint")
	// ********************************************************************************
	vppConn, vppErrCh := vpphelper.StartAndDialContext(ctx)
	exitOnErr(ctx, cancel, vppErrCh)

	firewallEndpoint := new(struct{ endpoint.Endpoint })
	firewallEndpoint.Endpoint = endpoint.NewServer(ctx,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		endpoint.WithName(config.Name),
		endpoint.WithAuthorizeServer(authorize.NewServer()),
		endpoint.WithAdditionalFunctionality(
			recvfd.NewServer(),
			sendfd.NewServer(),
			up.NewServer(ctx, vppConn),
			clienturl.NewServer(&config.ConnectTo),
			xconnect.NewServer(vppConn),
			acl.NewServer(vppConn, config.ACLConfig),
			mechanisms.NewServer(map[string]networkservice.NetworkServiceServer{
				memif.MECHANISM: chain.NewNetworkServiceServer(memif.NewServer(ctx, vppConn)),
			}),
			connect.NewServer(
				client.NewClient(
					ctx,
					client.WithoutRefresh(),
					client.WithName(config.Name),
					client.WithDialOptions(clientOptions...),
					client.WithAdditionalFunctionality(
						metadata.NewClient(),
						mechanismtranslation.NewClient(),
						replacelabels.NewClient(config.Labels),
						up.NewClient(ctx, vppConn),
						xconnect.NewClient(vppConn),
						memif.NewClient(vppConn),
						sendfd.NewClient(),
						recvfd.NewClient(),
					)),
			),
		))
	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 5: create grpc server and register firewall-server")
	// ********************************************************************************
	server := grpc.NewServer(append(
		opentracing.WithTracing(),
		grpc.Creds(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny()),
				),
			),
		),
	)...)
	firewallEndpoint.Register(server)

	tmpDir, err := ioutil.TempDir("", config.Name)
	if err != nil {
		logrus.Fatalf("error creating tmpDir %+v", err)
	}
	defer func(tmpDir string) { _ = os.Remove(tmpDir) }(tmpDir)
	listenOn := &(url.URL{Scheme: "unix", Path: filepath.Join(tmpDir, config.ListenOn)})

	srvErrCh := grpcutils.ListenAndServe(ctx, listenOn, server)
	exitOnErr(ctx, cancel, srvErrCh)
	log.FromContext(ctx).Infof("grpc server started")

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 6: register nse with nsm")
	// ********************************************************************************

	nseRegistryClient := registryclient.NewNetworkServiceEndpointRegistryClient(ctx, &config.ConnectTo, registryclient.WithDialOptions(clientOptions...))
	nse, err := nseRegistryClient.Register(ctx, &registryapi.NetworkServiceEndpoint{
		Name:                config.Name,
		NetworkServiceNames: []string{config.ServiceName},
		NetworkServiceLabels: map[string]*registryapi.NetworkServiceLabels{
			config.ServiceName: {
				Labels: config.Labels,
			},
		},
		Url: listenOn.String(),
	})
	logrus.Infof("nse: %+v", nse)

	if err != nil {
		log.FromContext(ctx).Fatalf("unable to register nse %+v", err)
	}

	// ********************************************************************************
	log.FromContext(ctx).Infof("startup completed in %v", time.Since(starttime))
	// ********************************************************************************

	// wait for server to exit
	<-ctx.Done()
	<-vppErrCh
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
}

func (c *Config) retrieveACLRules(ctx context.Context) {
	logger := log.FromContext(ctx).WithField("acl", "config")

	raw, err := ioutil.ReadFile(filepath.Clean(c.ACLConfigPath))
	if err != nil {
		logger.Errorf("Error reading config file: %v", err)
		return
	}
	logger.Infof("Read config file successfully")

	var rv map[string]acl_types.ACLRule
	err = yaml.Unmarshal(raw, &rv)
	if err != nil {
		logger.Errorf("Error parsing config file: %v", err)
		return
	}
	logger.Infof("Parsed acl rules successfully")

	for _, v := range rv {
		c.ACLConfig = append(c.ACLConfig, v)
	}

	logger.Infof("Result rules:%v", c.ACLConfig)
}
