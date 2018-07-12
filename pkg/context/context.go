/*
Copyright 2017 The Kubernetes Authors.
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

package context

import (
	"sync"
	"time"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	informerv1 "k8s.io/client-go/informers/core/v1"
	informerv1beta1 "k8s.io/client-go/informers/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	backendconfigclient "k8s.io/ingress-gce/pkg/backendconfig/client/clientset/versioned"
	informerbackendconfig "k8s.io/ingress-gce/pkg/backendconfig/client/informers/externalversions/backendconfig/v1beta1"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/firewalls"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
)

// ControllerContext holds resources needed for the execution of all individual controllers.
type ControllerContext struct {
	KubeClient kubernetes.Interface
	Cloud      *gce.GCECloud

	ControllerContextConfig

	IngressInformer       cache.SharedIndexInformer
	ServiceInformer       cache.SharedIndexInformer
	BackendConfigInformer cache.SharedIndexInformer
	PodInformer           cache.SharedIndexInformer
	NodeInformer          cache.SharedIndexInformer
	EndpointInformer      cache.SharedIndexInformer

	ClusterNamer  *utils.Namer
	InstancePool  instances.NodePool
	BackendPool   backends.BackendPool
	L7Pool        loadbalancers.LoadBalancerPool
	FirewallPool  firewalls.SingleFirewallPool
	HealthChecker healthchecks.HealthChecker

	healthChecks map[string]func() error
	hcLock       sync.Mutex

	// Map of namespace => record.EventRecorder.
	recorders map[string]record.EventRecorder
}

// ControllerContextConfig holds configuration that is tunable via command-line args.
type ControllerContextConfig struct {
	NEGEnabled           bool
	BackendConfigEnabled bool
	// DefaultBackendSvcPortID is the ServicePortID for the system default backend.
	DefaultBackendSvcPortID utils.ServicePortID

	Namespace    string
	ResyncPeriod time.Duration
	// DefaultBackendHealthCheckPath is the default path used for the default backend health checks.
	DefaultBackendHealthCheckPath string
	// HealthCheckPath is the default path used for L7 health checks, eg: "/healthz".
	HealthCheckPath string
	// NodePortRanges are the ports to be whitelisted for the L7 firewall rule.
	NodePortRanges []string
}

// NewControllerContext returns a new ControllerContext.
func NewControllerContext(
	kubeClient kubernetes.Interface,
	backendConfigClient backendconfigclient.Interface,
	namer *utils.Namer,
	cloud *gce.GCECloud,
	config ControllerContextConfig) *ControllerContext {

	newIndexer := func() cache.Indexers {
		return cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	}
	context := &ControllerContext{
		KubeClient: kubeClient,
		Cloud:      cloud,
		ControllerContextConfig: config,
		IngressInformer:         informerv1beta1.NewIngressInformer(kubeClient, config.Namespace, config.ResyncPeriod, newIndexer()),
		ServiceInformer:         informerv1.NewServiceInformer(kubeClient, config.Namespace, config.ResyncPeriod, newIndexer()),
		PodInformer:             informerv1.NewPodInformer(kubeClient, config.Namespace, config.ResyncPeriod, newIndexer()),
		NodeInformer:            informerv1.NewNodeInformer(kubeClient, config.ResyncPeriod, newIndexer()),
		ClusterNamer:            namer,
		recorders:               map[string]record.EventRecorder{},
		healthChecks:            make(map[string]func() error),
	}
	if context.NEGEnabled {
		context.EndpointInformer = informerv1.NewEndpointsInformer(kubeClient, config.Namespace, config.ResyncPeriod, newIndexer())
	}
	if context.BackendConfigEnabled {
		context.BackendConfigInformer = informerbackendconfig.NewBackendConfigInformer(backendConfigClient, config.Namespace, config.ResyncPeriod, newIndexer())
	}

	context.InstancePool = instances.NewNodePool(cloud, namer)
	context.HealthChecker = healthchecks.NewHealthChecker(cloud, config.HealthCheckPath, config.DefaultBackendHealthCheckPath, namer, config.DefaultBackendSvcPortID.Service)
	context.BackendPool = backends.NewBackendPool(cloud, cloud, context.HealthChecker, context.InstancePool, namer, config.BackendConfigEnabled, true)
	context.L7Pool = loadbalancers.NewLoadBalancerPool(cloud, namer)
	context.FirewallPool = firewalls.NewFirewallPool(cloud, namer, gce.LoadBalancerSrcRanges(), config.NodePortRanges)

	return context
}

// TODO(rramkumar): Figure out how to get rid of this or make it cleaner.
// Note: Copied this over from the now dead cluster manager.
func (ctx *ControllerContext) Init(zl instances.ZoneLister, pp backends.ProbeProvider) {
	ctx.InstancePool.Init(zl)
	ctx.BackendPool.Init(pp)
}

// HasSynced returns true if all relevant informers has been synced.
func (ctx *ControllerContext) HasSynced() bool {
	funcs := []func() bool{
		ctx.IngressInformer.HasSynced,
		ctx.ServiceInformer.HasSynced,
		ctx.PodInformer.HasSynced,
		ctx.NodeInformer.HasSynced,
	}
	if ctx.EndpointInformer != nil {
		funcs = append(funcs, ctx.EndpointInformer.HasSynced)
	}
	if ctx.BackendConfigInformer != nil {
		funcs = append(funcs, ctx.BackendConfigInformer.HasSynced)
	}
	for _, f := range funcs {
		if !f() {
			return false
		}
	}
	return true
}

// Start all of the informers.
func (ctx *ControllerContext) Start(stopCh chan struct{}) {
	go ctx.IngressInformer.Run(stopCh)
	go ctx.ServiceInformer.Run(stopCh)
	go ctx.PodInformer.Run(stopCh)
	go ctx.NodeInformer.Run(stopCh)
	if ctx.EndpointInformer != nil {
		go ctx.EndpointInformer.Run(stopCh)
	}
	if ctx.BackendConfigInformer != nil {
		go ctx.BackendConfigInformer.Run(stopCh)
	}
}

func (ctx *ControllerContext) Recorder(ns string) record.EventRecorder {
	if rec, ok := ctx.recorders[ns]; ok {
		return rec
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(glog.Infof)
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{
		Interface: ctx.KubeClient.Core().Events(ns),
	})
	rec := broadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{Component: "loadbalancer-controller"})
	ctx.recorders[ns] = rec

	return rec
}

// AddHealthCheck registers function to be called for healthchecking.
func (ctx *ControllerContext) AddHealthCheck(id string, hc func() error) {
	ctx.hcLock.Lock()
	defer ctx.hcLock.Unlock()

	ctx.healthChecks[id] = hc
}

// HealthCheckResults contains a mapping of component -> health check results.
type HealthCheckResults map[string]error

// HealthCheck runs all registered healthcheck functions.
func (ctx *ControllerContext) HealthCheck() HealthCheckResults {
	ctx.hcLock.Lock()
	defer ctx.hcLock.Unlock()

	healthChecks := make(map[string]error)
	for component, f := range ctx.healthChecks {
		healthChecks[component] = f()
	}

	return healthChecks
}
