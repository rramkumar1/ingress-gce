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

package neg

import (
	"encoding/json"
	"fmt"
	"strings"

	istioV1alpha3 "istio.io/api/networking/v1alpha3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/neg/types"
)

const (
	batchSyncer       = NegSyncerType("batch")
	transactionSyncer = NegSyncerType("transaction")
)

// NegSyncerType represents the the neg syncer type
type NegSyncerType string

// negServicePorts returns the parsed ServicePorts from the annotation.
// knownPorts represents the known Port:TargetPort attributes of servicePorts
// that already exist on the service. This function returns an error if
// any of the parsed ServicePorts from the annotation is not in knownPorts.
func negServicePorts(ann *annotations.NegAnnotation, knownPorts types.SvcPortMap) (types.SvcPortMap, error) {
	portSet := make(types.SvcPortMap)
	var errList []error
	for port := range ann.ExposedPorts {
		// TODO: also validate ServicePorts in the exposed NEG annotation via webhook
		if _, ok := knownPorts[port]; !ok {
			errList = append(errList, fmt.Errorf("port %v specified in %q doesn't exist in the service", port, annotations.NEGAnnotationKey))
		}
		portSet[port] = knownPorts[port]
	}

	return portSet, utilerrors.NewAggregate(errList)
}

// castToDestinationRule cast Unstructured obj to istioV1alpha3.DestinationRule
// Return targetServiceNamespace, targetSeriveName(DestinationRule.Host), DestionationRule and error.
func castToDestinationRule(drus *unstructured.Unstructured) (string, string, *istioV1alpha3.DestinationRule, error) {
	drJSON, err := json.Marshal(drus.Object["spec"])
	if err != nil {
		return "", "", nil, err
	}

	dr := &istioV1alpha3.DestinationRule{}
	if err := json.Unmarshal(drJSON, &dr); err != nil {
		return "", "", nil, err
	}

	targetServiceNamespace := drus.GetNamespace()
	drHost := dr.Host
	if strings.Contains(dr.Host, ".") {
		// If the Host is using a full service name, Istio will ignore the destination rule
		// namespace and use the namespace in the full name. (e.g. "reviews"
		// instead of "reviews.default.svc.cluster.local")
		// For more info, please go to https://github.com/istio/api/blob/1.2.4/networking/v1alpha3/destination_rule.pb.go#L186
		rsl := strings.Split(dr.Host, ".")
		targetServiceNamespace = rsl[1]
		drHost = rsl[0]
	}

	return targetServiceNamespace, drHost, dr, nil
}

func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
