/*
Copyright 2015 The Kubernetes Authors.

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

package server

import (
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/klog"
)

// KubeletAuth implements AuthInterface
type KubeletAuth struct {
	// authenticator identifies the user for requests to the Kubelet API
	authenticator.Request
	// authorizerAttributeGetter builds authorization.Attributes for a request to the Kubelet API
	authorizer.RequestAttributesGetter
	// authorizer determines whether a given authorization.Attributes is allowed
	authorizer.Authorizer
}

// NewKubeletAuth returns a kubelet.AuthInterface composed of the given authenticator, attribute getter, and authorizer
func NewKubeletAuth(authenticator authenticator.Request, authorizerAttributeGetter authorizer.RequestAttributesGetter, authorizer authorizer.Authorizer) AuthInterface {
	return &KubeletAuth{authenticator, authorizerAttributeGetter, authorizer}
}

// NewNodeAuthorizerAttributesGetter creates a new authorizer.RequestAttributesGetter for the node.
func NewNodeAuthorizerAttributesGetter(nodeName types.NodeName) authorizer.RequestAttributesGetter {
	return nodeAuthorizerAttributesGetter{nodeName: nodeName}
}

type nodeAuthorizerAttributesGetter struct {
	nodeName types.NodeName
}

// subpath等于path（去除最后斜杆）
// path（去除最后斜杆）是subpath的前缀，且前缀是一个完整路径（斜杆分隔），比如说subpath是"/a/b", path是"/a"，"/a"是第一级路径
func isSubpath(subpath, path string) bool {
	path = strings.TrimSuffix(path, "/")
	return subpath == path || (strings.HasPrefix(subpath, path) && subpath[len(path)] == '/')
}

// GetRequestAttributes populates authorizer attributes for the requests to the kubelet API.
// Default attributes are: {apiVersion=v1,verb=<http verb from request>,resource=nodes,name=<node name>,subresource=proxy}
// More specific verb/resource is set for the following request patterns:
//    /stats/*   => verb=<api verb from request>, resource=nodes, name=<node name>, subresource=stats
//    /metrics/* => verb=<api verb from request>, resource=nodes, name=<node name>, subresource=metrics
//    /logs/*    => verb=<api verb from request>, resource=nodes, name=<node name>, subresource=log
//    /spec/*    => verb=<api verb from request>, resource=nodes, name=<node name>, subresource=spec
func (n nodeAuthorizerAttributesGetter) GetRequestAttributes(u user.Info, r *http.Request) authorizer.Attributes {

	apiVerb := ""
	switch r.Method {
	case "POST":
		apiVerb = "create"
	case "GET":
		apiVerb = "get"
	case "PUT":
		apiVerb = "update"
	case "PATCH":
		apiVerb = "patch"
	case "DELETE":
		apiVerb = "delete"
	}

	requestPath := r.URL.Path

	// Default attributes mirror the API attributes that would allow this access to the kubelet API
	attrs := authorizer.AttributesRecord{
		User:            u,
		Verb:            apiVerb,
		Namespace:       "",
		APIGroup:        "",
		APIVersion:      "v1",
		Resource:        "nodes",
		Subresource:     "proxy",
		Name:            string(n.nodeName),
		ResourceRequest: true,
		Path:            requestPath,
	}

	// Override subresource for specific paths
	// This allows subdividing access to the kubelet API
	switch {
	// requestPath匹配"/stats/*"
	case isSubpath(requestPath, statsPath):
		attrs.Subresource = "stats"
	// requestPath匹配"/metrics/*"
	case isSubpath(requestPath, metricsPath):
		attrs.Subresource = "metrics"
	// requestPath匹配"/logs/*"
	case isSubpath(requestPath, logsPath):
		// "log" to match other log subresources (pods/log, etc)
		attrs.Subresource = "log"
	// requestPath匹配"/spec/*"
	case isSubpath(requestPath, specPath):
		attrs.Subresource = "spec"
	}

	klog.V(5).Infof("Node request attributes: user=%#v attrs=%#v", attrs.GetUser(), attrs)

	return attrs
}
