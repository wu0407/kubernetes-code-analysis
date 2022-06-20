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
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/util/sets"
)

// DockerKeyring tracks a set of docker registry credentials, maintaining a
// reverse index across the registry endpoints. A registry endpoint is made
// up of a host (e.g. registry.example.com), but it may also contain a path
// (e.g. registry.example.com/foo) This index is important for two reasons:
// - registry endpoints may overlap, and when this happens we must find the
//   most specific match for a given image
// - iterating a map does not yield predictable results
type DockerKeyring interface {
	Lookup(image string) ([]AuthConfig, bool)
}

// BasicDockerKeyring is a trivial map-backed implementation of DockerKeyring
type BasicDockerKeyring struct {
	index []string
	creds map[string][]AuthConfig
}

// providersDockerKeyring is an implementation of DockerKeyring that
// materializes its dockercfg based on a set of dockerConfigProviders.
type providersDockerKeyring struct {
	Providers []DockerConfigProvider
}

// AuthConfig contains authorization information for connecting to a Registry
// This type mirrors "github.com/docker/docker/api/types.AuthConfig"
type AuthConfig struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`

	// Email is an optional value associated with the username.
	// This field is deprecated and will be removed in a later
	// version of docker.
	Email string `json:"email,omitempty"`

	ServerAddress string `json:"serveraddress,omitempty"`

	// IdentityToken is used to authenticate the user and get
	// an access token for the registry.
	IdentityToken string `json:"identitytoken,omitempty"`

	// RegistryToken is a bearer token to be sent to a registry
	RegistryToken string `json:"registrytoken,omitempty"`
}

// 将cfg DockerConfig添加到dk.index和dk.creds
func (dk *BasicDockerKeyring) Add(cfg DockerConfig) {
	if dk.index == nil {
		dk.index = make([]string, 0)
		dk.creds = make(map[string][]AuthConfig)
	}
	// 遍历cfg DockerConfig，标准化仓库地址（兼容"/v2/"或"/v1/"前缀，域名没有协议则添加"https://"前缀（为了url.Parse不报错），忽略路径为"/"）作为添加dk.index和dk.creds的key，并生成dk.creds的value
	for loc, ident := range cfg {
		creds := AuthConfig{
			Username: ident.Username,
			Password: ident.Password,
			Email:    ident.Email,
		}

		value := loc
		// DockerConfig里仓库地址不包含"https://"和"http://"前缀，则添加"https://"前缀
		if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
			value = "https://" + value
		}
		// 仓库地址字符串转成url.URL
		parsed, err := url.Parse(value)
		if err != nil {
			klog.Errorf("Entry %q in dockercfg invalid (%v), ignoring", loc, err)
			continue
		}

		// The docker client allows exact matches:
		//    foo.bar.com/namespace
		// Or hostname matches:
		//    foo.bar.com
		// It also considers /v2/  and /v1/ equivalent to the hostname
		// See ResolveAuthConfig in docker/registry/auth.go.
		effectivePath := parsed.Path
		// 如果仓库地址path包含"/v2/"或"/v1/"开头，则移除"/v2"和"/v1"前缀
		if strings.HasPrefix(effectivePath, "/v2/") || strings.HasPrefix(effectivePath, "/v1/") {
			effectivePath = effectivePath[3:]
		}
		var key string
		// 仓库地址path不为空且不为"/"，则key为域名加路径
		if (len(effectivePath) > 0) && (effectivePath != "/") {
			key = parsed.Host + effectivePath
		} else {
			key = parsed.Host
		}
		dk.creds[key] = append(dk.creds[key], creds)
		dk.index = append(dk.index, key)
	}

	// 对dk.index进行去重
	eliminateDupes := sets.NewString(dk.index...)
	dk.index = eliminateDupes.List()

	// Update the index used to identify which credentials to use for a given
	// image. The index is reverse-sorted so more specific paths are matched
	// first. For example, if for the given image "quay.io/coreos/etcd",
	// credentials for "quay.io/coreos" should match before "quay.io".
	// 对dk.index进行逆序排列，让仓库地址优先最长匹配
	sort.Sort(sort.Reverse(sort.StringSlice(dk.index)))
}

const (
	defaultRegistryHost = "index.docker.io"
	defaultRegistryKey  = defaultRegistryHost + "/v1/"
)

// isDefaultRegistryMatch determines whether the given image will
// pull from the default registry (DockerHub) based on the
// characteristics of its name.
// image是否是docker hub仓库地址或本地仓库
func isDefaultRegistryMatch(image string) bool {
	parts := strings.SplitN(image, "/", 2)

	// 不包含"/"，则返回false（因为这里image仓库地址已经是dockerref.ParseNormalizedNamed解析过了，不包含"/"的镜像地址会添加"library/"前缀）
	if len(parts[0]) == 0 {
		return false
	}

	if len(parts) == 1 {
		// e.g. library/ubuntu
		return true
	}

	if parts[0] == "docker.io" || parts[0] == "index.docker.io" {
		// resolve docker.io/image and index.docker.io/image as default registry
		return true
	}

	// From: http://blog.docker.com/2013/07/how-to-use-your-own-registry/
	// Docker looks for either a “.” (domain separator) or “:” (port separator)
	// to learn that the first part of the repository name is a location and not
	// a user name.
	return !strings.ContainsAny(parts[0], ".:")
}

// url.Parse require a scheme, but ours don't have schemes.  Adding a
// scheme to make url.Parse happy, then clear out the resulting scheme.
// 对schemelessUrl添加"https://"前缀，然后进行url.Parse，然后清空*url.URL里的Scheme，返回*url.URL
// 解析schemelessUrl为Scheme空的*url.URL
func parseSchemelessUrl(schemelessUrl string) (*url.URL, error) {
	parsed, err := url.Parse("https://" + schemelessUrl)
	if err != nil {
		return nil, err
	}
	// clear out the resulting scheme
	parsed.Scheme = ""
	return parsed, nil
}

// split the host name into parts, as well as the port
// 将url.Host解析成主机名和端口，返回主机名使用"."进行分隔后的[]string和端口
func splitUrl(url *url.URL) (parts []string, port string) {
	host, port, err := net.SplitHostPort(url.Host)
	if err != nil {
		// could not parse port
		host, port = url.Host, ""
	}
	return strings.Split(host, "."), port
}

// overloaded version of urlsMatch, operating on strings instead of URLs.
// glob是否能够统配符匹配target
func urlsMatchStr(glob string, target string) (bool, error) {
	// 解析schemelessUrl为Scheme空的*url.URL
	globUrl, err := parseSchemelessUrl(glob)
	if err != nil {
		return false, err
	}
	targetUrl, err := parseSchemelessUrl(target)
	if err != nil {
		return false, err
	}
	// 检查globUrl是否能够通配符匹配targetUrl
	return urlsMatch(globUrl, targetUrl)
}

// check whether the given target url matches the glob url, which may have
// glob wild cards in the host name.
//
// Examples:
//    globUrl=*.docker.io, targetUrl=blah.docker.io => match
//    globUrl=*.docker.io, targetUrl=not.right.io   => no match
//
// Note that we don't support wildcards in ports and paths yet.
// 检查globUrl是否能够通配符匹配targetUrl
func urlsMatch(globUrl *url.URL, targetUrl *url.URL) (bool, error) {
	// 将globUrl.Host解析成主机名和端口，返回主机名使用"."进行分隔后的[]string和端口
	globUrlParts, globPort := splitUrl(globUrl)
	// 将targetUrl.Host解析成主机名和端口，返回主机名使用"."进行分隔后的[]string和端口
	targetUrlParts, targetPort := splitUrl(targetUrl)
	// 端口不匹配，直接返回false, nil
	if globPort != targetPort {
		// port doesn't match
		return false, nil
	}
	// 不是同一子域，则返回false, nil
	if len(globUrlParts) != len(targetUrlParts) {
		// host name does not have the same number of parts
		return false, nil
	}
	// globUrl.Path不是targetUrl.Path前缀，直接返回false, nil
	// 比如globUrl.Path是"/a/b"，而targetUrl.Path是"/a/c/d"
	if !strings.HasPrefix(targetUrl.Path, globUrl.Path) {
		// the path of the credential must be a prefix
		return false, nil
	}
	// 遍历globUrl每级域，是否每一级域都是通配符匹配
	for k, globUrlPart := range globUrlParts {
		targetUrlPart := targetUrlParts[k]
		matched, err := filepath.Match(globUrlPart, targetUrlPart)
		// globUrlPart语法不对
		if err != nil {
			return false, err
		}
		// 不匹配则直接返回false, nil
		if !matched {
			// glob mismatch for some part
			return false, nil
		}
	}
	// everything matches
	return true, nil
}

// Lookup implements the DockerKeyring method for fetching credentials based on image name.
// Multiple credentials may be returned if there are multiple potentially valid credentials
// available.  This allows for rotation.
// 遍历dk *BasicDockerKeyring中所有的凭证，找到匹配image仓库地址的AuthConfig凭证列表
func (dk *BasicDockerKeyring) Lookup(image string) ([]AuthConfig, bool) {
	// range over the index as iterating over a map does not provide a predictable ordering
	ret := []AuthConfig{}
	// 遍历所有仓库名，找到能够匹配image镜像地址的AuthConfig
	for _, k := range dk.index {
		// both k and image are schemeless URLs because even though schemes are allowed
		// in the credential configurations, we remove them in Add.
		// dk.Add里添加到dk.index的key是没有scheme的url，而且镜像地址image也是没有scheme的url
		// k是否能够统配符匹配image
		if matched, _ := urlsMatchStr(k, image); matched {
			ret = append(ret, dk.creds[k]...)
		}
	}

	// 找到匹配的AuthConfig，直接返回
	if len(ret) > 0 {
		return ret, true
	}

	// Use credentials for the default registry if provided, and appropriate
	// 未找到匹配的AuthConfig，则检查image是否是docker hub仓库地址或本地仓库
	if isDefaultRegistryMatch(image) {
		// 如果是dockerhub仓库，则使用"index.docker.io"仓库的AuthConfig凭证
		if auth, ok := dk.creds[defaultRegistryHost]; ok {
			return auth, true
		}
	}

	return []AuthConfig{}, false
}

// Lookup implements the DockerKeyring method for fetching credentials
// based on image name.
// 遍历所有注册并启用的provider提供的凭证，找到匹配的image仓库地址的凭证列表和是否找到匹配凭证
func (dk *providersDockerKeyring) Lookup(image string) ([]AuthConfig, bool) {
	keyring := &BasicDockerKeyring{}

	for _, p := range dk.Providers {
		// 如果provider是CachingDockerConfigProvider
		// p.Provide(image)
		// 从缓存未过期则从d.cacheDockerConfig缓存中获取DockerConfig，直接返回
		// 否则
		//   先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
		//   没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
		//   上面都没有，则返回空的DockerConfig
		// 更新缓存和过期时间
		// keyring.Add
		// 将cfg DockerConfig添加到BasicDockerKeyring dk.index和dk.creds
		keyring.Add(p.Provide(image))
	}

	// 遍历dk *BasicDockerKeyring中所有的凭证，找到匹配image仓库地址的AuthConfig凭证列表
	return keyring.Lookup(image)
}

type FakeKeyring struct {
	auth []AuthConfig
	ok   bool
}

func (f *FakeKeyring) Lookup(image string) ([]AuthConfig, bool) {
	return f.auth, f.ok
}

// UnionDockerKeyring delegates to a set of keyrings.
type UnionDockerKeyring []DockerKeyring

// 遍历UnionDockerKeyring里所有的DockerKeyring（实现），调用Lookup查找image仓库地址的凭证列表
// 返回所有找到image仓库地址的凭证列表和是否找到匹配凭证
func (k UnionDockerKeyring) Lookup(image string) ([]AuthConfig, bool) {
	authConfigs := []AuthConfig{}
	for _, subKeyring := range k {
		if subKeyring == nil {
			continue
		}

		currAuthResults, _ := subKeyring.Lookup(image)
		authConfigs = append(authConfigs, currAuthResults...)
	}

	return authConfigs, (len(authConfigs) > 0)
}
