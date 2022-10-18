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

package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletconfigv1beta1 "k8s.io/kubernetes/pkg/kubelet/apis/config/v1beta1"
)

// Utility functions for the Kubelet's kubeletconfig API group

// NewSchemeAndCodecs is a utility function that returns a Scheme and CodecFactory
// that understand the types in the kubeletconfig API group. Passing mutators allows
// for adjusting the behavior of the CodecFactory, for example enable strict decoding.
// 将（pkg/kubelet/apis/config）schema.GroupVersion{Group: "kubelet.config.k8s.io", Version: "__internal"}下的KubeletConfiguration{}和SerializedNodeConfigSource{}添加到scheme中
// 将（staging\src\k8s.io\kubelet\config\v1beta1）schema.GroupVersion{Group: "kubelet.config.k8s.io", Version: "v1beta1"}下的KubeletConfiguration{}和SerializedNodeConfigSource{}添加到scheme中
// 返回scheme和codec和错误
func NewSchemeAndCodecs(mutators ...serializer.CodecFactoryOptionsMutator) (*runtime.Scheme, *serializer.CodecFactory, error) {
	scheme := runtime.NewScheme()
	// 将（pkg/kubelet/apis/config）schema.GroupVersion{Group: "kubelet.config.k8s.io", Version: "__internal"}下的KubeletConfiguration{}和SerializedNodeConfigSource{}添加到scheme中
	if err := kubeletconfig.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	// 将（staging\src\k8s.io\kubelet\config\v1beta1）schema.GroupVersion{Group: "kubelet.config.k8s.io", Version: "v1beta1"}下的KubeletConfiguration{}和SerializedNodeConfigSource{}添加到scheme中
	if err := kubeletconfigv1beta1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	codecs := serializer.NewCodecFactory(scheme, mutators...)
	return scheme, &codecs, nil
}
