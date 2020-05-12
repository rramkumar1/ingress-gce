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

package translator

import (
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ServicePort is a helper function that retrieves a port of a Service.
func ServicePort(svc api_v1.Service, port intstr.IntOrString) *api_v1.ServicePort {
	var svcPort *api_v1.ServicePort
PortLoop:
	for _, p := range svc.Spec.Ports {
		np := p
		switch port.Type {
		case intstr.Int:
			if p.Port == port.IntVal {
				svcPort = &np
				break PortLoop
			}
		default:
			if p.Name == port.StrVal {
				svcPort = &np
				break PortLoop
			}
		}
	}
	return svcPort
}

// maybeEnableNEG enables NEG on the service port if necessary
func maybeEnableNEG(sp *utils.ServicePort, svc *api_v1.Service) error {
	negAnnotation, ok, err := annotations.FromService(svc).NEGAnnotation()
	if ok && err == nil {
		sp.NEGEnabled = negAnnotation.NEGEnabledForIngress()
	}

	if !sp.NEGEnabled && svc.Spec.Type != api_v1.ServiceTypeNodePort &&
		svc.Spec.Type != api_v1.ServiceTypeLoadBalancer {
		// This is a fatal error.
		return errors.ErrBadSvcType{Service: sp.ID.Service, ServiceType: svc.Spec.Type}
	}

	if sp.L7ILBEnabled {
		// L7-ILB Requires NEGs
		sp.NEGEnabled = true
	}

	return nil
}

// setAppProtocol sets the app protocol on the service port
func setAppProtocol(sp *utils.ServicePort, svc *api_v1.Service, port *api_v1.ServicePort) error {
	appProtocols, err := annotations.FromService(svc).ApplicationProtocols()
	if err != nil {
		return errors.ErrSvcAppProtosParsing{Service: sp.ID.Service, Err: err}
	}

	proto := annotations.ProtocolHTTP
	if protoStr, exists := appProtocols[port.Name]; exists {
		proto = annotations.AppProtocol(protoStr)
	}
	sp.Protocol = proto

	return nil
}

// maybeEnableBackendConfig sets the backendConfig for the service port if necessary
func (t *Translator) maybeEnableBackendConfig(sp *utils.ServicePort, svc *api_v1.Service, port *api_v1.ServicePort) error {
	var beConfig *backendconfigv1.BackendConfig
	beConfig, err := backendconfig.GetBackendConfigForServicePort(t.ctx.BackendConfigInformer.GetIndexer(), svc, port)
	if err != nil {
		// If we could not find a backend config name for the current
		// service port, then do not return an error. Removing a reference
		// to a backend config from the service annotation is a valid
		// step that a user could take.
		if err != backendconfig.ErrNoBackendConfigForPort {
			return errors.ErrSvcBackendConfig{ServicePortID: sp.ID, Err: err}
		}
	}
	// Object in cache could be changed in-flight. Deepcopy to
	// reduce race conditions.
	beConfig = beConfig.DeepCopy()
	if err = backendconfig.Validate(t.ctx.KubeClient, beConfig); err != nil {
		return errors.ErrBackendConfigValidation{BackendConfig: *beConfig, Err: err}
	}

	sp.BackendConfig = beConfig
	return nil
}

// getServicePortParams allows for passing parameters to getServicePort()
type getServicePortParams struct {
	isL7ILB bool
}

// getServicePort looks in the svc store for a matching service:port,
// and returns the nodeport.
func (t *Translator) getServicePort(id utils.ServicePortID, params *getServicePortParams, namer namer_util.BackendNamer) (*utils.ServicePort, error) {
	svc, err := t.getCachedService(id)
	if err != nil {
		return nil, err
	}

	port := ServicePort(*svc, id.Port)
	if port == nil {
		// This is a fatal error.
		return nil, errors.ErrSvcPortNotFound{ServicePortID: id}
	}

	// We periodically add information to the ServicePort to ensure that we
	// always return as much as possible, rather than nil, if there was a non-fatal error.
	svcPort := &utils.ServicePort{
		ID:           id,
		NodePort:     int64(port.NodePort),
		Port:         int32(port.Port),
		TargetPort:   port.TargetPort.String(),
		L7ILBEnabled: params.isL7ILB,
		BackendNamer: namer,
	}

	if err := maybeEnableNEG(svcPort, svc); err != nil {
		return nil, err
	}

	if err := setAppProtocol(svcPort, svc, port); err != nil {
		return svcPort, err
	}

	if err := t.maybeEnableBackendConfig(svcPort, svc, port); err != nil {
		return svcPort, err
	}

	return svcPort, nil
}

func (t *Translator) getCachedService(id utils.ServicePortID) (*api_v1.Service, error) {
	obj, exists, err := t.ctx.ServiceInformer.GetIndexer().Get(
		&api_v1.Service{
			ObjectMeta: meta_v1.ObjectMeta{
				Name:      id.Service.Name,
				Namespace: id.Service.Namespace,
			},
		})
	if !exists {
		// This is a fatal error.
		return nil, errors.ErrSvcNotFound{Service: id.Service}
	}
	if err != nil {
		// This is a fatal error.
		return nil, fmt.Errorf("error retrieving service %q: %v", id.Service, err)
	}

	svc, ok := obj.(*api_v1.Service)
	if !ok {
		return nil, fmt.Errorf("cannot convert to Service (%T)", svc)
	}
	return svc, nil
}
