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

package resource

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// TODO require negotiatedSerializer.  leaving it optional lets us plumb current behavior and deal with the difference after major plumbing is complete
// 执行clientConfigFn()生成*rest.Config
// 如果negotiatedSerializer不为nil，设置（*rest.Config）.ContentConfig.NegotiatedSerializer为negotiatedSerializer
// 设置（*rest.Config）.GroupVersion为gv，根据gv.Group设置cfg.APIPath为"/api"或"/apis"
// 调用rest.RESTClientFor(*rest.Config)，生成*rest.RESTClient
func (clientConfigFn ClientConfigFunc) clientForGroupVersion(gv schema.GroupVersion, negotiatedSerializer runtime.NegotiatedSerializer) (RESTClient, error) {
	cfg, err := clientConfigFn()
	if err != nil {
		return nil, err
	}
	if negotiatedSerializer != nil {
		cfg.ContentConfig.NegotiatedSerializer = negotiatedSerializer
	}
	cfg.GroupVersion = &gv
	if len(gv.Group) == 0 {
		cfg.APIPath = "/api"
	} else {
		cfg.APIPath = "/apis"
	}

	return rest.RESTClientFor(cfg)
}

// 执行clientConfigFn()生成*rest.Config
// cfg.ContentConfig为(rest.ContentConfig).AcceptContentTypes为"application/json"，(rest.ContentConfig).ContentType为"application/json"，(rest.ContentConfig).NegotiatedSerializer为将runtime.Object解析成unstructured或metav1.Status
// 设置（*rest.Config）.GroupVersion为gv，根据gv.Group设置cfg.APIPath为"/api"或"/apis"
// 调用rest.RESTClientFor(*rest.Config)，生成*rest.RESTClient
func (clientConfigFn ClientConfigFunc) unstructuredClientForGroupVersion(gv schema.GroupVersion) (RESTClient, error) {
	cfg, err := clientConfigFn()
	if err != nil {
		return nil, err
	}
	// (rest.ContentConfig).AcceptContentTypes为"application/json"，(rest.ContentConfig).ContentType为"application/json"
	// (rest.ContentConfig).NegotiatedSerializer为将runtime.Object解析成unstructured或metav1.Status
	cfg.ContentConfig = UnstructuredPlusDefaultContentConfig()
	cfg.GroupVersion = &gv
	if len(gv.Group) == 0 {
		cfg.APIPath = "/api"
	} else {
		cfg.APIPath = "/apis"
	}

	return rest.RESTClientFor(cfg)
}

// 包装clientConfigFn
// 如果stdinUnavailable为true，且clientConfigFn()生成rest.Config不为nil，且cfg.ExecProvider不为nil
// 设置（rest.Config）.ExecProvider.StdinUnavailable为true，（rest.Config）.ExecProvider.StdinUnavailableMessage为"used by stdin resource manifest reader"
func (clientConfigFn ClientConfigFunc) withStdinUnavailable(stdinUnavailable bool) ClientConfigFunc {
	return func() (*rest.Config, error) {
		cfg, err := clientConfigFn()
		if stdinUnavailable && cfg != nil && cfg.ExecProvider != nil {
			cfg.ExecProvider.StdinUnavailable = stdinUnavailable
			cfg.ExecProvider.StdinUnavailableMessage = "used by stdin resource manifest reader"
		}
		return cfg, err
	}
}
