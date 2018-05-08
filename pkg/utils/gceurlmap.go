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

package utils

import "fmt"

// GCEURLMap is a simple internal representation of compute.UrlMap
type GCEURLMap struct {
	// DefaultBackendName is the k8s name given to the default backend.
	DefaultBackendName string
	// hostRules is a mapping from a hostname to its associated rules.
	hostRules map[string][]PathRule
}

// PathRule encapsulates the information for a single path rule for a host.
type PathRule struct {
	// Path is a regex.
	Path string
	// BackendName is the k8s name given to the backend.
	BackendName string
}

// PutPathRulesForHost adds path rules for a single hostname. Multiple calls
// to this function with the same hostname will result in overwriting behavior.
func (g *GCEURLMap) PutPathRulesForHost(hostname string, rules map[string]string) {
	pathRules := make([]PathRule, 0)
	for path, backendName := range rules {
		pathRules = append(pathRules, PathRule{path, backendName})
	}
	g.hostRules[hostname] = pathRules
}

// AllRules returns every list of PathRule's for each hostname.
func (g *GCEURLMap) AllRules() map[string][]PathRule {
	return g.hostRules
}

// String dumps a readable version of the GCEURLMap.
func (g *GCEURLMap) String() string {
	msg := ""
	for host, rules := range g.hostRules {
		msg += fmt.Sprintf("%v\n", host)
		for _, rule := range rules {
			msg += fmt.Sprintf("\t%v: ", rule.Path)
			if rule.BackendName == "" {
				msg += fmt.Sprintf("No backend\n")
			} else {
				msg += fmt.Sprintf("%v\n", rule.BackendName)
			}
		}
	}
	msg += fmt.Sprintf("Default Backend: %v", g.DefaultBackendName)
	return msg
}
