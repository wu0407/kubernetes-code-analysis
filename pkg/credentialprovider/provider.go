/*
Copyright 2014 The Kubernetes Authors.

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

package credentialprovider

import (
	"os"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog"
)

// DockerConfigProvider is the interface that registered extensions implement
// to materialize 'dockercfg' credentials.
type DockerConfigProvider interface {
	// Enabled returns true if the config provider is enabled.
	// Implementations can be blocking - e.g. metadata server unavailable.
	Enabled() bool
	// Provide returns docker configuration.
	// Implementations can be blocking - e.g. metadata server unavailable.
	// The image is passed in as context in the event that the
	// implementation depends on information in the image name to return
	// credentials; implementations are safe to ignore the image.
	Provide(image string) DockerConfig
}

// A DockerConfigProvider that simply reads the .dockercfg file
type defaultDockerConfigProvider struct{}

// init registers our default provider, which simply reads the .dockercfg file.
func init() {
	RegisterCredentialProvider(".dockercfg",
		&CachingDockerConfigProvider{
			Provider: &defaultDockerConfigProvider{},
			Lifetime: 5 * time.Minute,
		})
}

// CachingDockerConfigProvider implements DockerConfigProvider by composing
// with another DockerConfigProvider and caching the DockerConfig it provides
// for a pre-specified lifetime.
type CachingDockerConfigProvider struct {
	Provider DockerConfigProvider
	Lifetime time.Duration

	// ShouldCache is an optional function that returns true if the specific config should be cached.
	// If nil, all configs are treated as cacheable.
	ShouldCache func(DockerConfig) bool

	// cache fields
	cacheDockerConfig DockerConfig
	expiration        time.Time
	mu                sync.Mutex
}

// Enabled implements dockerConfigProvider
func (d *defaultDockerConfigProvider) Enabled() bool {
	return true
}

// Provide implements dockerConfigProvider
// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
// 上面都没有，则返回空的DockerConfig
func (d *defaultDockerConfigProvider) Provide(image string) DockerConfig {
	// Read the standard Docker credentials from .dockercfg
	// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
	// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
	if cfg, err := ReadDockerConfigFile(); err == nil {
		return cfg
	} else if !os.IsNotExist(err) {
		klog.V(4).Infof("Unable to parse Docker config file: %v", err)
	}
	return DockerConfig{}
}

// Enabled implements dockerConfigProvider
func (d *CachingDockerConfigProvider) Enabled() bool {
	return d.Provider.Enabled()
}

// Provide implements dockerConfigProvider
// 从缓存未过期则从d.cacheDockerConfig缓存中获取DockerConfig
// 否则
//   先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
//   没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
//   上面都没有，则返回空的DockerConfig
// 更新缓存和过期时间
func (d *CachingDockerConfigProvider) Provide(image string) DockerConfig {
	d.mu.Lock()
	defer d.mu.Unlock()

	// If the cache hasn't expired, return our cache
	// 缓存未过期，则返回缓存的数据
	if time.Now().Before(d.expiration) {
		return d.cacheDockerConfig
	}

	// 缓存过期了
	klog.V(2).Infof("Refreshing cache for provider: %v", reflect.TypeOf(d.Provider).String())
	// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
	// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
	// 上面都没有，则返回空的DockerConfig
	config := d.Provider.Provide(image)
	// d.ShouldCache未定义或d.ShouldCache(config)返回true
	if d.ShouldCache == nil || d.ShouldCache(config) {
		// 设置缓存
		d.cacheDockerConfig = config
		// 设置过期时间
		d.expiration = time.Now().Add(d.Lifetime)
	}
	return config
}
