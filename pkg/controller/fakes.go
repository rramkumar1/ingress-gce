/*
Copyright 2018 The Kubernetes Authors.
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

package controller

import (
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/ingress-gce/pkg/annotations"
	backendconfigclient "k8s.io/ingress-gce/pkg/backendconfig/client/clientset/versioned"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/firewalls"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/neg"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
)

var (
	clusterName          = "mycluster"
	testBackendPort      = intstr.IntOrString{Type: intstr.Int, IntVal: 80}
	testDefaultBeSvcPort = utils.ServicePort{
		ID:       utils.ServicePortID{Service: types.NamespacedName{Namespace: "system", Name: "default"}, Port: testBackendPort},
		NodePort: 30000,
		Protocol: annotations.ProtocolHTTP,
	}
	testNodePortRanges = []string{"30000-32767"}
	testSrcRanges      = []string{"1.1.1.1/20"}
)

func NewFakeLoadBalancerController() *LoadBalancerController {
	kubeClient := fake.NewSimpleClientset()
	backendConfigClient := backendconfigclient.NewSimpleClientset()
	namer := utils.NewNamer(clusterName, "")
	cloud := gce.FakeGCECloud(gce.DefaultTestClusterValues())

	ctx := context.NewControllerContext(kubeClient, backendConfigClient, namer, cloud, config)

	fakeIGs := instances.NewFakeInstanceGroups(sets.NewString(), namer)
	fakeNEG := neg.NewFakeNetworkEndpointGroupCloud("test-subnet", "test-network")
	fakeHCP := healthchecks.NewFakeHealthCheckProvider()
	nodePool := instances.NewNodePool(fakeIGs, namer)
	nodePool.Init(&instances.FakeZoneLister{Zones: []string{"zone-a"}})

	// Override resource pools with fakes
	ctx.InstancePool = instances.NewFakeInstanceGroups(sets.NewString(), namer)
	ctx.HealthChecker = healthchecks.NewHealthChecker(fakeHCP, "/", "/healthz", namer, testDefaultBeSvcPort.ID.Service)
	ctx.BackendPool = backends.NewBackendPool(cloud, fakeNEG, context.healthChecker, nodePool, namer, false, false)
	ctx.L7Pool = loadbalancers.NewFakeLoadBalancers(clusterName, namer)
	ctx.FirewallPool = firewalls.NewFirewallPool(firewalls.NewFakeFirewallsProvider(false, false), namer, testSrcRanges, testNodePortRanges)

	lbc, err := NewLoadBalancerController(ctx, stopCh)
	lbc.hasSynced = func() bool { return true }

	return lbc
}
