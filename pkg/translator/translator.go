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

package translator

import (
	"fmt"
	"sort"
	"strconv"

	"k8s.io/ingress-gce/pkg/flags"

	"k8s.io/klog"

	api_v1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"k8s.io/ingress-gce/pkg/annotations"
	backendconfigv1 "k8s.io/ingress-gce/pkg/apis/backendconfig/v1"
	"k8s.io/ingress-gce/pkg/backendconfig"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/controller/errors"
	"k8s.io/ingress-gce/pkg/utils"
	namer_util "k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/ingress-gce/pkg/composite/composite"
)

const (
	// DefaultHost is the host used if none is specified. It is a valid value
	// for the "Host" field recognized by GCE.
	DefaultHost = "*"

	// DefaultPath is the path used if none is specified. It is a valid path
	// recognized by GCE.
	DefaultPath = "/*"
)

// Env contains all k8s-style configuration needed to perform the translation.
type Env struct {
	// MCI is the MCI we are translating.
	Ing *v1beta1.Ingress
	// ServiceMap contains a mapping from Service name to the actual resource.
	// It is assumed that the map contains resources from a single namespace.
	ServiceMap map[string]*api_v1.Service
	// SecretsMap contains a mapping from Secret name to the actual resource.
	// It is assumed that the map contains resources from a single namespace.
	SecretsMap map[string]*api_v1.Secret
	// BackendConfigs contains a mapping from the service name to the backend config CRD.
	BackendConfigs map[string]*backendconfigv1.BackendConfig
}

// BackendLookup is an interface to get backend for a given service port.
type BackendLookup interface {
	GroupKeys(id serviceport.ID) ([]*composite.Backend, error)
}

type AddressLookup interface {
	Address(

// RetryOptions encapsulates options for translation retries.
type RetryOptions struct {
	MaxRetries int
	Backoff    time.Duration
}

// Translator implements the mapping between an MCI and its corresponding GCE resources.
type Translator struct {
	backendNamer      namer.BackendNamer
	v1FrontendNamer   namer.V1FrontendNamer
	frontendNamer     namer.IngressFrontendNamer
	backendLookup BackendLookup
	retryOpts         RetryOptions
}

// NewTranslator returns a new Translator.
func NewTranslator(backendNamer namer.BackendNamer, v1FrontendNamer namer.V1FrontendNamer, frontendNamer namer.IngressFrontendNamer, backendLookup BackendLookup, retryOpts RetryOptions) *Translator {
	return &Translator{}
}

// TranslateIngress converts an Ingress into a load balancer specification that can be synced to GCE.
// It attempts a certain amount of retries if there are errors during the translation but only the
// latest error is returned. This could result in a non-trivial amount of blocking.
func (t *Translator) TranslateIngress(env *Env) (*composite.LoadBalancer, error) {
	var lb *composite.LoadBalancer
	var retries int

	b := backoff.NewConstantBackOff(t.retryOpts.Backoff)
	f := func() error {
		var err error
		lb, err = t.translateIngress(env)
		if err != nil {
			retries++
			if retries > t.retryOpts.MaxRetries {
				return backoff.Permanent(err)
			}
			return err
		}
		return nil
	}
	if permErr := backoff.Retry(f, b); permErr != nil {
		return nil, permErr
	}

	return lb, nil
}

func (t *Translator) translateIngress(env *Env) (*composite.LoadBalancer, error) {
	lb := &composite.LoadBalancer{}
	mciKey := gkenet.NewKey(env.MCI.Namespace, env.MCI.Name)

	g, err := t.toInternal(env)
	if err != nil {
		return nil, err
	}
	secrets, err := secrets(env)
	if err != nil {
		return nil, err
	}
	vip := computeVIP(env.MCI)
	certs := preSharedCerts(env.MCI)

	t.addURLMap(mciKey, g, lb)
	t.addFrontendResources(mciKey, vip, secrets, certs, lb)

	allSvcPorts := g.AllServicePorts()
	for svcPort := range allSvcPorts {
		gks, err := t.negGroupKeyLookup.GroupKeys(svcPort.ID)
		if err != nil {
			return nil, errors.MissingNEGError(svcPort.ID)
		}

		mcsKey := gkenet.NewKey(svcPort.ID.Key.Namespace, svcPort.ID.Key.Name)
		bc, err := backendConfig(env, mcsKey.Name, svcPort.ID)
		if err != nil {
			return nil, err
		}
		t.addBackendServiceAndHealthCheck(mcsKey, &svcPort, bc, negBackends(gks), lb)
	}

	return lb, nil
}

// TranslateIngress converts an Ingress into our internal UrlMap representation.
func (t *Translator) toInternal(ing *v1beta1.Ingress, systemDefaultBackend utils.ServicePortID) (*utils.GCEURLMap, []error) {
	var errs []error
	urlMap := utils.NewGCEURLMap()

	params := &getServicePortParams{}
	params.isL7ILB = flags.F.EnableL7Ilb && utils.IsGCEL7ILBIngress(ing)

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		pathRules := []utils.PathRule{}
		for _, p := range rule.HTTP.Paths {
			svcPort, err := t.getServicePort(utils.BackendToServicePortID(p.Backend, ing.Namespace), params)
			if err != nil {
				errs = append(errs, err)
			}
			if svcPort != nil {
				// The Ingress spec defines empty path as catch-all, so if a user
				// asks for a single host and multiple empty paths, all traffic is
				// sent to one of the last backend in the rules list.
				path := p.Path
				if path == "" {
					path = DefaultPath
				}
				pathRules = append(pathRules, utils.PathRule{Path: path, Backend: *svcPort})
			}
		}
		host := rule.Host
		if host == "" {
			host = DefaultHost
		}
		urlMap.PutPathRulesForHost(host, pathRules)
	}

	if ing.Spec.Backend != nil {
		svcPort, err := t.getServicePort(utils.BackendToServicePortID(*ing.Spec.Backend, ing.Namespace), params)
		if err == nil {
			urlMap.DefaultBackend = svcPort
			return urlMap, errs
		}

		errs = append(errs, err)
		return urlMap, errs
	}

	svcPort, err := t.getServicePort(systemDefaultBackend, params)
	if err == nil {
		urlMap.DefaultBackend = svcPort
		return urlMap, errs
	}

	errs = append(errs, fmt.Errorf("failed to retrieve the system default backend service %q with port %q: %v", systemDefaultBackend.Service.String(), systemDefaultBackend.Port.String(), err))
	return urlMap, errs{
}

func (t *Translator) addURLMap(mciKey gkenet.Key, g *GCEURLMap, lb *composite.LoadBalancer) {
	defaultBackendName := t.namer.BackendService(g.DefaultBackend.ID.Key, g.DefaultBackend.ID.PortNumber)
	um := &composite.UrlMap{
		Name:           t.namer.URLMap(mciKey),
		DefaultService: cloud.NewBackendServicesResourceID("", defaultBackendName).ResourcePath(),
	}

	for _, hostRule := range g.HostRules {
		// Create a host rule
		// Create a path matcher
		// Add all given endpoint:backends to pathRules in path matcher
		pmName := pathMatcherName(hostRule.Hostname)
		um.HostRules = append(um.HostRules, &gspb.HostRule{
			Hosts:       []string{hostRule.Hostname},
			PathMatcher: pmName,
		})
		pathMatcher := &gspb.PathMatcher{
			Name:           pmName,
			DefaultService: um.DefaultService,
			PathRules:      []*gspb.PathRule{},
		}
		// GCE ensures that matched rule with longest prefix wins.
		for _, rule := range hostRule.Paths {
			beName := t.namer.BackendService(rule.Backend.ID.Key, rule.Backend.ID.PortNumber)
			pathMatcher.PathRules = append(pathMatcher.PathRules, &gspb.PathRule{
				Paths:   []string{rule.Path},
				Service: cloud.NewBackendServicesResourceID("", beName).ResourcePath(),
			})
		}
		um.PathMatchers = append(um.PathMatchers, pathMatcher)
	}

	lb.UrlMaps = append(lb.UrlMaps, um)
}

func (t *Translator) addFrontendResources(ip string, secrets []*corev1.Secret, preSharedCerts []string, lb *composite.LoadBalancer) {
	tpName := t.namer.TargetProxy(mciKey, false)
	umName := t.namer.URLMap(mciKey)
	lb.TargetHttpProxies = append(lb.TargetHttpProxies, &gspb.TargetHttpProxy{
		Name:   tpName,
		UrlMap: cloud.NewUrlMapsResourceID("", umName).ResourcePath(),
	})

	lb.ForwardingRules = append(lb.ForwardingRules, &gspb.ForwardingRule{
		Name:                t.namer.ForwardingRule(mciKey, false),
		IpAddress:           ip,
		Target:              cloud.NewTargetHttpProxiesResourceID("", tpName).ResourcePath(),
		PortRange:           fwHTTPPortRange,
		IpProtocol:          fwProtocol,
		LoadBalancingScheme: lbScheme,
	})
	for _, network := range t.networks {
		lb.Firewalls = append(lb.Firewalls, &gspb.Firewall{
			Name:         t.namer.Firewall(network),
			Description:  "MCI L7 firewall rule",
			SourceRanges: healthcheck.ProdSourceRanges,
			Allowed: []*gspb.FirewallAllowed{
				{
					// TODO(rramkumar): Restrict ports to just the ones we need and just the instances we need.
					IpProtocol: "tcp",
					Ports:      fullPortRange,
				},
			},
			Network: cloud.NewNetworksResourceID("", network).ResourcePath(),
		})
	}

	if len(secrets) == 0 && len(preSharedCerts) == 0 {
		return
	}

	var sslCertNames []string
	for _, secret := range secrets {
		key := gkenet.NewKey(secret.Namespace, secret.Name)
		cert := string(secret.Data[corev1.TLSCertKey])
		sslCert := &gspb.SslCertificate{
			Name:        t.namer.SslCertificate(key, cert),
			Certificate: cert,
			PrivateKey:  string(secret.Data[corev1.TLSPrivateKeyKey]),
		}
		lb.SslCertificates = append(lb.SslCertificates, sslCert)
		sslCertNames = append(sslCertNames, cloud.NewSslCertificatesResourceID("", sslCert.Name).ResourcePath())
	}

	for _, cert := range preSharedCerts {
		sslCertNames = append(sslCertNames, cloud.NewSslCertificatesResourceID("", cert).ResourcePath())
	}

	tpsName := t.namer.TargetProxy(mciKey, true)
	lb.TargetHttpsProxies = append(lb.TargetHttpsProxies, &gspb.TargetHttpsProxy{
		Name:            tpsName,
		UrlMap:          cloud.NewUrlMapsResourceID("", umName).ResourcePath(),
		SslCertificates: sslCertNames,
	})
	lb.ForwardingRules = append(lb.ForwardingRules, &gspb.ForwardingRule{
		Name:                t.namer.ForwardingRule(mciKey, true),
		IpAddress:           ip,
		Target:              cloud.NewTargetHttpsProxiesResourceID("", tpsName).ResourcePath(),
		PortRange:           fwHTTPSPortRange,
		IpProtocol:          fwProtocol,
		LoadBalancingScheme: lbScheme,
	})

}

// secrets returns the Secrets from the environment which are specified in the MCI.
func secrets(env *Env) ([]*corev1.Secret, error) {
	var ret []*corev1.Secret
	mciSpec := env.MCI.Spec.Template.Spec
	for _, tlsSpec := range mciSpec.TLS {
		secret, ok := env.SecretsMap[tlsSpec.SecretName]
		if !ok {
			return nil, errors.MissingResourceError("Secret", env.MCI.Namespace, tlsSpec.SecretName)
		}
		// Fail-fast if the user's secret does not have the proper fields specified.
		if secret.Data[corev1.TLSCertKey] == nil {
			return nil, fmt.Errorf("secret %q does not specify cert as string data", tlsSpec.SecretName)
		}
		if secret.Data[corev1.TLSPrivateKeyKey] == nil {
			return nil, fmt.Errorf("secret %q does not specify private key as string data", tlsSpec.SecretName)
		}
		ret = append(ret, secret)
	}

	return ret, nil
}

// GetBackendConfigForServicePort returns the corresponding BackendConfig for
// the given ServicePort if specified.
func GetBackendConfigForServicePort(backendConfigLister cache.Store, svc *apiv1.Service, svcPort *apiv1.ServicePort) (*backendconfigv1.BackendConfig, error) {
	backendConfigs, err := annotations.FromService(svc).GetBackendConfigs()
	if err != nil {
		// If the user did not provide the annotation at all, then we
		// do not want to return an error.
		if err == annotations.ErrBackendConfigAnnotationMissing {
			return nil, nil
		}
		return nil, err
	}

	configName := BackendConfigName(*backendConfigs, *svcPort)
	if configName == "" {
		return nil, ErrNoBackendConfigForPort
	}

	obj, exists, err := backendConfigLister.Get(
		&backendconfigv1.BackendConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configName,
				Namespace: svc.Namespace,
			},
		})
	if err != nil {
		return nil, ErrBackendConfigFailedToGet
	}
	if !exists {
		return nil, ErrBackendConfigDoesNotExist
	}

	return obj.(*backendconfigv1.BackendConfig), nil
}

func (t *Translator) addBackendServiceAndHealthCheck() {}

// geHTTPProbe returns the http readiness probe from the first container
// that matches targetPort, from the set of pods matching the given labels.
func (t *Translator) getHTTPProbe(svc api_v1.Service, targetPort intstr.IntOrString, protocol annotations.AppProtocol) (*api_v1.Probe, error) {
	l := svc.Spec.Selector

	// Lookup any container with a matching targetPort from the set of pods
	// with a matching label selector.
	pl, err := listPodsBySelector(t.ctx.PodInformer.GetIndexer(), labels.SelectorFromSet(labels.Set(l)))
	if err != nil {
		return nil, err
	}

	// If multiple endpoints have different health checks, take the first
	sort.Sort(orderedPods(pl))

	for _, pod := range pl {
		if pod.Namespace != svc.Namespace {
			continue
		}
		logStr := fmt.Sprintf("Pod %v matching service selectors %v (targetport %+v)", pod.Name, l, targetPort)
		for _, c := range pod.Spec.Containers {
			if !isSimpleHTTPProbe(c.ReadinessProbe) || getProbeScheme(protocol) != c.ReadinessProbe.HTTPGet.Scheme {
				continue
			}

			for _, p := range c.Ports {
				if (targetPort.Type == intstr.Int && targetPort.IntVal == p.ContainerPort) ||
					(targetPort.Type == intstr.String && targetPort.StrVal == p.Name) {

					readinessProbePort := c.ReadinessProbe.Handler.HTTPGet.Port
					switch readinessProbePort.Type {
					case intstr.Int:
						if readinessProbePort.IntVal == p.ContainerPort {
							return c.ReadinessProbe, nil
						}
					case intstr.String:
						if readinessProbePort.StrVal == p.Name {
							return c.ReadinessProbe, nil
						}
					}

					klog.Infof("%v: found matching targetPort on container %v, but not on readinessProbe (%+v)",
						logStr, c.Name, c.ReadinessProbe.Handler.HTTPGet.Port)
				}
			}
		}
		klog.V(5).Infof("%v: lacks a matching HTTP probe for use in health checks.", logStr)
	}
	return nil, nil
}

// GatherEndpointPorts returns all ports needed to open NEG endpoints.
func (t *Translator) GatherEndpointPorts(svcPorts []utils.ServicePort) []string {
	portMap := map[int64]bool{}
	for _, p := range svcPorts {
		if p.NEGEnabled {
			// For NEG backend, need to open firewall to all endpoint target ports
			// TODO(mixia): refactor firewall syncing into a separate go routine with different trigger.
			// With NEG, endpoint changes may cause firewall ports to be different if user specifies inconsistent backends.
			endpointPorts := listEndpointTargetPorts(t.ctx.EndpointInformer.GetIndexer(), p.ID.Service.Namespace, p.ID.Service.Name, p.TargetPort)
			for _, ep := range endpointPorts {
				portMap[int64(ep)] = true
			}
		}
	}
	var portStrs []string
	for p := range portMap {
		portStrs = append(portStrs, strconv.FormatInt(p, 10))
	}
	return portStrs
}

// isSimpleHTTPProbe returns true if the given Probe is:
// - an HTTPGet probe, as opposed to a tcp or exec probe
// - has no special host or headers fields, except for possibly an HTTP Host header
func isSimpleHTTPProbe(probe *api_v1.Probe) bool {
	return (probe != nil && probe.Handler.HTTPGet != nil && probe.Handler.HTTPGet.Host == "" &&
		(len(probe.Handler.HTTPGet.HTTPHeaders) == 0 ||
			(len(probe.Handler.HTTPGet.HTTPHeaders) == 1 && probe.Handler.HTTPGet.HTTPHeaders[0].Name == "Host")))
}

// getProbeScheme returns the Kubernetes API URL scheme corresponding to the
// protocol.
func getProbeScheme(protocol annotations.AppProtocol) api_v1.URIScheme {
	if protocol == annotations.ProtocolHTTP2 {
		return api_v1.URISchemeHTTPS
	}
	return api_v1.URIScheme(string(protocol))
}

// GetProbe returns a probe that's used for the given nodeport
func (t *Translator) GetProbe(port utils.ServicePort) (*api_v1.Probe, error) {
	sl := t.ctx.ServiceInformer.GetIndexer().List()

	// Find the label and target port of the one service with the given nodePort
	var service api_v1.Service
	var svcPort api_v1.ServicePort
	var found bool
OuterLoop:
	for _, as := range sl {
		service = *as.(*api_v1.Service)
		for _, sp := range service.Spec.Ports {
			svcPort = sp
			// If service is NEG enabled, compare the service name and namespace instead
			// This is because NEG enabled service is not required to have nodePort
			if port.NEGEnabled && port.ID.Service.Namespace == service.Namespace && port.ID.Service.Name == service.Name && port.Port == sp.Port {
				found = true
				break OuterLoop
			}
			// only one Service can match this nodePort, try and look up
			// the readiness probe of the pods behind it
			if port.NodePort != 0 && int32(port.NodePort) == sp.NodePort {
				found = true
				break OuterLoop
			}
		}
	}

	if !found {
		return nil, fmt.Errorf("unable to find nodeport %v in any service", port)
	}

	return t.getHTTPProbe(service, svcPort.TargetPort, port.Protocol)
}

// listPodsBySelector returns a list of all pods based on selector
func listPodsBySelector(indexer cache.Indexer, selector labels.Selector) (ret []*api_v1.Pod, err error) {
	err = listAll(indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*api_v1.Pod))
	})
	return ret, err
}

// listAll iterates a store and passes selected item to a func
func listAll(store cache.Store, selector labels.Selector, appendFn cache.AppendFunc) error {
	for _, m := range store.List() {
		metadata, err := meta.Accessor(m)
		if err != nil {
			return err
		}
		if selector.Matches(labels.Set(metadata.GetLabels())) {
			appendFn(m)
		}
	}
	return nil
}

func listEndpointTargetPorts(indexer cache.Indexer, namespace, name, targetPort string) []int {
	// if targetPort is integer, no need to translate to endpoint ports
	if i, err := strconv.Atoi(targetPort); err == nil {
		return []int{i}
	}

	ep, exists, err := indexer.Get(
		&api_v1.Endpoints{
			ObjectMeta: meta_v1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		},
	)

	if !exists {
		klog.Errorf("Endpoint object %v/%v does not exist.", namespace, name)
		return []int{}
	}
	if err != nil {
		klog.Errorf("Failed to retrieve endpoint object %v/%v: %v", namespace, name, err)
		return []int{}
	}

	ret := []int{}
	for _, subset := range ep.(*api_v1.Endpoints).Subsets {
		for _, port := range subset.Ports {
			if port.Protocol == api_v1.ProtocolTCP && port.Name == targetPort {
				ret = append(ret, int(port.Port))
			}
		}
	}
	return ret
}

// orderedPods sorts a list of Pods by creation timestamp, using their names as a tie breaker.
type orderedPods []*api_v1.Pod

func (o orderedPods) Len() int      { return len(o) }
func (o orderedPods) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o orderedPods) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}
