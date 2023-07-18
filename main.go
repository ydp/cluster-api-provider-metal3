/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	bmov1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	infrav1alpha5 "github.com/metal3-io/cluster-api-provider-metal3/api/v1alpha5"
	infrav1 "github.com/metal3-io/cluster-api-provider-metal3/api/v1beta1"
	"github.com/metal3-io/cluster-api-provider-metal3/baremetal"
	infraremote "github.com/metal3-io/cluster-api-provider-metal3/baremetal/remote"
	"github.com/metal3-io/cluster-api-provider-metal3/controllers"
	ipamv1 "github.com/metal3-io/ip-address-manager/api/v1alpha1"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	caipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	// +kubebuilder:scaffold:imports
)

type TLSVersion string

// Constants for TLS versions.
const (
	TLSVersion12 TLSVersion = "TLS12"
	TLSVersion13 TLSVersion = "TLS13"
)

type TLSOptions struct {
	TLSMaxVersion   string
	TLSMinVersion   string
	TLSCipherSuites string
}

var (
	myscheme                         = runtime.NewScheme()
	setupLog                         = ctrl.Log.WithName("setup")
	waitForMetal3Controller          = false
	metricsBindAddr                  string
	enableLeaderElection             bool
	leaderElectionLeaseDuration      time.Duration
	leaderElectionRenewDeadline      time.Duration
	leaderElectionRetryPeriod        time.Duration
	syncPeriod                       time.Duration
	metal3MachineConcurrency         int
	metal3ClusterConcurrency         int
	metal3DataTemplateConcurrency    int
	metal3DataConcurrency            int
	metal3LabelSyncConcurrency       int
	metal3MachineTemplateConcurrency int
	metal3RemediationConcurrency     int
	webhookPort                      int
	webhookCertDir                   string
	healthAddr                       string
	watchNamespace                   string
	watchFilterValue                 string
	logOptions                       = logs.NewOptions()
	enableBMHNameBasedPreallocation  bool
	tlsOptions                       = TLSOptions{}
	tlsSupportedVersions             = []string{"TLS12", "TLS13"}
)

func init() {
	_ = scheme.AddToScheme(myscheme)
	_ = ipamv1.AddToScheme(myscheme)
	_ = caipamv1.AddToScheme(myscheme)
	_ = infrav1.AddToScheme(myscheme)
	_ = infrav1alpha5.AddToScheme(myscheme)
	_ = clusterv1.AddToScheme(myscheme)
	_ = bmov1alpha1.AddToScheme(myscheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	rand.Seed(time.Now().UnixNano())
	initFlags(pflag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctrl.SetLogger(klogr.New())
	restConfig := ctrl.GetConfigOrDie()
	restConfig.UserAgent = "cluster-api-provider-metal3-manager"

	tlsOptionOverrides, err := GetTLSOptionOverrideFuncs(tlsOptions)
	if err != nil {
		setupLog.Error(err, "unable to add TLS settings to the webhook server")
		os.Exit(1)
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                     myscheme,
		MetricsBindAddress:         metricsBindAddr,
		LeaseDuration:              &leaderElectionLeaseDuration,
		RenewDeadline:              &leaderElectionRenewDeadline,
		RetryPeriod:                &leaderElectionRetryPeriod,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           "controller-leader-election-capm3",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		SyncPeriod:                 &syncPeriod,
		Port:                       webhookPort,
		CertDir:                    webhookCertDir,
		HealthProbeBindAddress:     healthAddr,
		Namespace:                  watchNamespace,
		TLSOpts:                    tlsOptionOverrides,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if waitForMetal3Controller {
		err = waitForAPIs(ctrl.GetConfigOrDie())
		if err != nil {
			setupLog.Error(err, "unable to discover required APIs")
			os.Exit(1)
		}
	}

	// Setup the context that's going to be used in controllers and for the manager.
	ctx := ctrl.SetupSignalHandler()

	if enableBMHNameBasedPreallocation {
		baremetal.EnableBMHNameBasedPreallocation = enableBMHNameBasedPreallocation
	}

	setupChecks(mgr)
	setupReconcilers(ctx, mgr)
	setupWebhooks(mgr)

	// +kubebuilder:scaffold:builder
	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(fs *pflag.FlagSet) {
	logs.AddFlags(fs, logs.SkipLoggingConfigurationFlags())
	logsv1.AddFlags(logOptions, fs)

	fs.StringVar(
		&metricsBindAddr,
		"metrics-bind-addr",
		"localhost:8080",
		"The address the metric endpoint binds to.",
	)

	fs.BoolVar(
		&enableLeaderElection,
		"leader-elect",
		false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.",
	)

	fs.BoolVar(
		&enableBMHNameBasedPreallocation,
		"enableBMHNameBasedPreallocation",
		false,
		"If set to true, it enables PreAllocation field to use Metal3IPClaim name structured with BaremetalHost and M3IPPool names",
	)

	fs.DurationVar(
		&leaderElectionLeaseDuration,
		"leader-elect-lease-duration",
		15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)",
	)

	fs.DurationVar(
		&leaderElectionRenewDeadline,
		"leader-elect-renew-deadline",
		10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)",
	)

	fs.DurationVar(
		&leaderElectionRetryPeriod,
		"leader-elect-retry-period",
		2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)",
	)

	fs.StringVar(
		&watchNamespace,
		"namespace",
		"",
		"Namespace that the controller watches to reconcile CAPM3 objects. If unspecified, the controller watches for CAPM3 objects across all namespaces.",
	)

	fs.StringVar(
		&watchFilterValue,
		"watch-filter",
		"",
		fmt.Sprintf("Label value that the controller watches to reconcile cluster-api objects. Label key is always %s. If unspecified, the controller watches for all cluster-api objects.", clusterv1.WatchLabel),
	)

	fs.DurationVar(
		&syncPeriod,
		"sync-period",
		10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)",
	)

	fs.IntVar(
		&webhookPort,
		"webhook-port",
		9443,
		"Webhook Server port",
	)

	fs.StringVar(
		&webhookCertDir,
		"webhook-cert-dir",
		"/tmp/k8s-webhook-server/serving-certs/",
		"Webhook cert dir, only used when webhook-port is specified.",
	)

	fs.StringVar(
		&healthAddr,
		"health-addr",
		":9440",
		"The address the health endpoint binds to.",
	)

	fs.IntVar(&metal3MachineConcurrency, "metal3machine-concurrency", 1,
		"Number of metal3machines to process simultaneously. WARNING! Currently not safe to set > 1.")

	fs.IntVar(&metal3ClusterConcurrency, "metal3cluster-concurrency", 10,
		"Number of metal3clusters to process simultaneously")

	fs.IntVar(&metal3DataTemplateConcurrency, "metal3datatemplate-concurrency", 10,
		"Number of metal3datatemplates to process simultaneously")

	fs.IntVar(&metal3DataConcurrency, "metal3data-concurrency", 10,
		"Number of metal3data to process simultaneously")

	fs.IntVar(&metal3LabelSyncConcurrency, "metal3labelsync-concurrency", 10,
		"Number of metal3labelsyncs to process simultaneously")

	fs.IntVar(&metal3MachineTemplateConcurrency, "metal3machinetemplate-concurrency", 10,
		"Number of metal3machinetemplates to process simultaneously")

	fs.IntVar(&metal3RemediationConcurrency, "metal3remediation-concurrency", 10,
		"Number of metal3remediations to process simultaneously")

	flag.StringVar(&tlsOptions.TLSMinVersion, "tls-min-version", "TLS12",
		"The minimum TLS version in use by the webhook server.\n"+
			fmt.Sprintf("Possible values are %s.", strings.Join(tlsSupportedVersions, ", ")),
	)

	fs.StringVar(&tlsOptions.TLSMaxVersion, "tls-max-version", "TLS13",
		"The maximum TLS version in use by the webhook server.\n"+
			fmt.Sprintf("Possible values are %s.", strings.Join(tlsSupportedVersions, ", ")),
	)

	tlsCipherPreferredValues := cliflag.PreferredTLSCipherNames()
	tlsCipherInsecureValues := cliflag.InsecureTLSCipherNames()
	fs.StringVar(&tlsOptions.TLSCipherSuites, "tls-cipher-suites", "",
		"Comma-separated list of cipher suites for the webhook server. "+
			"If omitted, the default Go cipher suites will be used. \n"+
			"Preferred values: "+strings.Join(tlsCipherPreferredValues, ", ")+". \n"+
			"Insecure values: "+strings.Join(tlsCipherInsecureValues, ", ")+".")
}

func waitForAPIs(cfg *rest.Config) error {
	c, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}

	metal3GV := schema.GroupVersion{
		Group:   "metal3.io",
		Version: "v1alpha1",
	}

	for {
		err = discovery.ServerSupportsVersion(c, metal3GV)
		if err != nil {
			setupLog.Info(fmt.Sprintf("Waiting for API group %v to be available: %v", metal3GV, err))
			time.Sleep(time.Second * 10)
			continue
		}
		setupLog.Info(fmt.Sprintf("Found API group %v", metal3GV))
		break
	}

	return nil
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}
}

func setupReconcilers(ctx context.Context, mgr ctrl.Manager) {
	if err := (&controllers.Metal3MachineReconciler{
		Client:           mgr.GetClient(),
		ManagerFactory:   baremetal.NewManagerFactory(mgr.GetClient()),
		Log:              ctrl.Log.WithName("controllers").WithName("Metal3Machine"),
		CapiClientGetter: infraremote.NewClusterClient,
		WatchFilterValue: watchFilterValue,
	}).SetupWithManager(ctx, mgr, concurrency(metal3MachineConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3MachineReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3ClusterReconciler{
		Client:           mgr.GetClient(),
		ManagerFactory:   baremetal.NewManagerFactory(mgr.GetClient()),
		Log:              ctrl.Log.WithName("controllers").WithName("Metal3Cluster"),
		WatchFilterValue: watchFilterValue,
	}).SetupWithManager(ctx, mgr, concurrency(metal3ClusterConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3ClusterReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3DataTemplateReconciler{
		Client:           mgr.GetClient(),
		ManagerFactory:   baremetal.NewManagerFactory(mgr.GetClient()),
		Log:              ctrl.Log.WithName("controllers").WithName("Metal3DataTemplate"),
		WatchFilterValue: watchFilterValue,
	}).SetupWithManager(ctx, mgr, concurrency(metal3DataTemplateConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3DataTemplateReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3DataReconciler{
		Client:           mgr.GetClient(),
		ManagerFactory:   baremetal.NewManagerFactory(mgr.GetClient()),
		Log:              ctrl.Log.WithName("controllers").WithName("Metal3Data"),
		WatchFilterValue: watchFilterValue,
	}).SetupWithManager(ctx, mgr, concurrency(metal3DataConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3DataReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3LabelSyncReconciler{
		Client:           mgr.GetClient(),
		ManagerFactory:   baremetal.NewManagerFactory(mgr.GetClient()),
		Log:              ctrl.Log.WithName("controllers").WithName("Metal3LabelSync"),
		CapiClientGetter: infraremote.NewClusterClient,
	}).SetupWithManager(ctx, mgr, concurrency(metal3LabelSyncConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3LabelSyncReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3MachineTemplateReconciler{
		Client:         mgr.GetClient(),
		ManagerFactory: baremetal.NewManagerFactory(mgr.GetClient()),
		Log:            ctrl.Log.WithName("controllers").WithName("Metal3MachineTemplate"),
	}).SetupWithManager(ctx, mgr, concurrency(metal3MachineTemplateConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3MachineTemplateReconciler")
		os.Exit(1)
	}

	if err := (&controllers.Metal3RemediationReconciler{
		Client:         mgr.GetClient(),
		ManagerFactory: baremetal.NewManagerFactory(mgr.GetClient()),
		Log:            ctrl.Log.WithName("controllers").WithName("Metal3Remediation"),
	}).SetupWithManager(ctx, mgr, concurrency(metal3RemediationConcurrency)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Metal3Remediation")
		os.Exit(1)
	}
}

func setupWebhooks(mgr ctrl.Manager) {
	if err := (&infrav1.Metal3Cluster{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3Cluster")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3Machine{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3Machine")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3MachineTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3MachineTemplate")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3DataTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3DataTemplate")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3Data{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3Data")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3DataClaim{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3DataClaim")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3Remediation{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3Remediation")
		os.Exit(1)
	}

	if err := (&infrav1.Metal3RemediationTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Metal3RemediationTemplate")
		os.Exit(1)
	}
}

func concurrency(c int) controller.Options {
	return controller.Options{MaxConcurrentReconciles: c}
}

// GetTLSOptionOverrideFuncs returns a list of TLS configuration overrides to be used
// by the webhook server.
func GetTLSOptionOverrideFuncs(options TLSOptions) ([]func(*tls.Config), error) {
	var tlsOptions []func(config *tls.Config)

	tlsMinVersion, err := GetTLSVersion(options.TLSMinVersion)
	if err != nil {
		return nil, err
	}

	tlsMaxVersion, err := GetTLSVersion(options.TLSMaxVersion)
	if err != nil {
		return nil, err
	}

	if tlsMaxVersion != 0 && tlsMinVersion > tlsMaxVersion {
		return nil, fmt.Errorf("TLS version flag min version (%s) is greater than max version (%s)",
			options.TLSMinVersion, options.TLSMaxVersion)
	}

	tlsOptions = append(tlsOptions, func(cfg *tls.Config) {
		cfg.MinVersion = tlsMinVersion
	})

	tlsOptions = append(tlsOptions, func(cfg *tls.Config) {
		cfg.MaxVersion = tlsMaxVersion
	})
	// Cipher suites should not be set if empty.
	if options.TLSMinVersion == string(TLSVersion13) &&
		options.TLSMaxVersion == string(TLSVersion13) &&
		options.TLSCipherSuites != "" {
		setupLog.Info("warning: Cipher suites should not be set for TLS version 1.3. Ignoring ciphers")
		options.TLSCipherSuites = ""
	}

	if options.TLSCipherSuites != "" {
		tlsCipherSuites := strings.Split(options.TLSCipherSuites, ",")
		suites, err := cliflag.TLSCipherSuites(tlsCipherSuites)
		if err != nil {
			return nil, err
		}

		insecureCipherValues := cliflag.InsecureTLSCipherNames()
		for _, cipher := range tlsCipherSuites {
			for _, insecureCipherName := range insecureCipherValues {
				if insecureCipherName == cipher {
					setupLog.Info(fmt.Sprintf("warning: use of insecure cipher '%s' detected.", cipher))
				}
			}
		}
		tlsOptions = append(tlsOptions, func(cfg *tls.Config) {
			cfg.CipherSuites = suites
		})
	}

	return tlsOptions, nil
}

// GetTLSVersion returns the corresponding tls.Version or error.
func GetTLSVersion(version string) (uint16, error) {
	var v uint16

	switch version {
	case string(TLSVersion12):
		v = tls.VersionTLS12
	case string(TLSVersion13):
		v = tls.VersionTLS13
	default:
		return 0, fmt.Errorf("unexpected TLS version %q (must be one of: TLS12, TLS13)", version)
	}
	return v, nil
}
