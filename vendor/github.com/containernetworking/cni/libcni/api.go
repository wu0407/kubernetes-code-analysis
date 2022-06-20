// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package libcni

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
)

var (
	CacheDir = "/var/lib/cni"
)

// A RuntimeConf holds the arguments to one invocation of a CNI plugin
// excepting the network configuration, with the nested exception that
// the `runtimeConfig` from the network configuration is included
// here.
type RuntimeConf struct {
	ContainerID string
	NetNS       string
	IfName      string
	Args        [][2]string
	// A dictionary of capability-specific data passed by the runtime
	// to plugins as top-level keys in the 'runtimeConfig' dictionary
	// of the plugin's stdin data.  libcni will ensure that only keys
	// in this map which match the capabilities of the plugin are passed
	// to the plugin
	CapabilityArgs map[string]interface{}

	// A cache directory in which to library data.  Defaults to CacheDir
	CacheDir string
}

type NetworkConfig struct {
	Network *types.NetConf
	Bytes   []byte
}

type NetworkConfigList struct {
	Name         string
	CNIVersion   string
	DisableCheck bool
	Plugins      []*NetworkConfig
	Bytes        []byte
}

type CNI interface {
	AddNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) (types.Result, error)
	CheckNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) error
	DelNetworkList(ctx context.Context, net *NetworkConfigList, rt *RuntimeConf) error
	GetNetworkListCachedResult(net *NetworkConfigList, rt *RuntimeConf) (types.Result, error)

	AddNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) (types.Result, error)
	CheckNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error
	DelNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error
	GetNetworkCachedResult(net *NetworkConfig, rt *RuntimeConf) (types.Result, error)

	ValidateNetworkList(ctx context.Context, net *NetworkConfigList) ([]string, error)
	ValidateNetwork(ctx context.Context, net *NetworkConfig) ([]string, error)
}

// 用于查找插件和执行插件
type CNIConfig struct {
	Path []string
	exec invoke.Exec
}

// CNIConfig implements the CNI interface
var _ CNI = &CNIConfig{}

// NewCNIConfig returns a new CNIConfig object that will search for plugins
// in the given paths and use the given exec interface to run those plugins,
// or if the exec interface is not given, will use a default exec handler.
func NewCNIConfig(path []string, exec invoke.Exec) *CNIConfig {
	return &CNIConfig{
		Path: path,
		exec: exec,
	}
}

// 让cniVersion和name覆盖orig.CNIVersion和orig.name，prevResult（参数）赋值给orig.RawPrevResult字段，orig.Bytes字段里会有"prevResult": prevResult(参数转成[]byte)
// 添加key为"runtimeConfig"，value为orig.Network.Capabilities中启用的Capability的配置（在rt.CapabilityArgs中）到orig.Bytes里
func buildOneConfig(name, cniVersion string, orig *NetworkConfig, prevResult types.Result, rt *RuntimeConf) (*NetworkConfig, error) {
	var err error

	inject := map[string]interface{}{
		"name":       name,
		"cniVersion": cniVersion,
	}
	// Add previous plugin result
	if prevResult != nil {
		inject["prevResult"] = prevResult
	}

	// Ensure every config uses the same name and version
	// inject里的字段覆盖orig.Bytes里的相应字段
	orig, err = InjectConf(orig, inject)
	if err != nil {
		return nil, err
	}

	return injectRuntimeConfig(orig, rt)
}

// This function takes a libcni RuntimeConf structure and injects values into
// a "runtimeConfig" dictionary in the CNI network configuration JSON that
// will be passed to the plugin on stdin.
//
// Only "capabilities arguments" passed by the runtime are currently injected.
// These capabilities arguments are filtered through the plugin's advertised
// capabilities from its config JSON, and any keys in the CapabilityArgs
// matching plugin capabilities are added to the "runtimeConfig" dictionary
// sent to the plugin via JSON on stdin.  For example, if the plugin's
// capabilities include "portMappings", and the CapabilityArgs map includes a
// "portMappings" key, that key and its value are added to the "runtimeConfig"
// dictionary to be passed to the plugin's stdin.
// 添加key为"runtimeConfig"，value为orig.Network.Capabilities中启用的Capability的配置（在rt.CapabilityArgs中）到orig.Bytes里
// 比如{"runtimeConfig":{"ipRanges":[[{"subnet":"10.248.0.0/25"}]],"portMappings":[]}}
func injectRuntimeConfig(orig *NetworkConfig, rt *RuntimeConf) (*NetworkConfig, error) {
	var err error

	rc := make(map[string]interface{})
	for capability, supported := range orig.Network.Capabilities {
		if !supported {
			continue
		}
		if data, ok := rt.CapabilityArgs[capability]; ok {
			rc[capability] = data
		}
	}

	if len(rc) > 0 {
		orig, err = InjectConf(orig, map[string]interface{}{"runtimeConfig": rc})
		if err != nil {
			return nil, err
		}
	}

	return orig, nil
}

// ensure we have a usable exec if the CNIConfig was not given one
func (c *CNIConfig) ensureExec() invoke.Exec {
	if c.exec == nil {
		c.exec = &invoke.DefaultExec{
			RawExec:       &invoke.RawExec{Stderr: os.Stderr},
			PluginDecoder: version.PluginDecoder{},
		}
	}
	return c.exec
}

// 返回{cacheDir}/results/{netName}/{container id}/{rt.ifName}，比如/var/lib/cni/cache/results/kubenet-loopback-761f2c6dc1503077db58e85e19873758aae73d2fae4b920f9fe24674c1db920a-lo
func getResultCacheFilePath(netName string, rt *RuntimeConf) string {
	cacheDir := rt.CacheDir
	if cacheDir == "" {
		cacheDir = CacheDir
	}
	return filepath.Join(cacheDir, "results", fmt.Sprintf("%s-%s-%s", netName, rt.ContainerID, rt.IfName))
}

// 默认在/var/lib/cni/cache/results/下生成保存插件执行输出的文件
// 比如文件名为kubenet-761f2c6dc1503077db58e85e19873758aae73d2fae4b920f9fe24674c1db920a-eth0，内容{"cniVersion":"0.2.0","ip4":{"ip":"10.252.0.143/25","gateway":"10.252.0.129","routes":[{"dst":"0.0.0.0/0"}]},"dns":{}}
// 比如文件名为kubenet-loopback-761f2c6dc1503077db58e85e19873758aae73d2fae4b920f9fe24674c1db920a-lo，内容为{"cniVersion":"0.2.0","ip4":{"ip":"127.0.0.1/8"},"dns":{}}
func setCachedResult(result types.Result, netName string, rt *RuntimeConf) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fname := getResultCacheFilePath(netName, rt)
	if err := os.MkdirAll(filepath.Dir(fname), 0700); err != nil {
		return err
	}
	return ioutil.WriteFile(fname, data, 0600)
}

// 删除{cacheDir}/results/{netName}/{container id}/{ifName}文件，容器相关的cache result结果缓存文件
func delCachedResult(netName string, rt *RuntimeConf) error {
	// 返回{cacheDir}/results/{netName}/{container id}/{ifName}，比如/var/lib/cni/cache/results/kubenet-loopback-761f2c6dc1503077db58e85e19873758aae73d2fae4b920f9fe24674c1db920a-lo
	fname := getResultCacheFilePath(netName, rt)
	return os.Remove(fname)
}

// 从cni的缓存目录中获取cniVersion版本的result，如果文件不存在，则忽略这个错误，返回Result为nil，err为nil
func getCachedResult(netName, cniVersion string, rt *RuntimeConf) (types.Result, error) {
	// 返回{cacheDir}/results/{netName}/{container id}/{rt.ifName}，比如/var/lib/cni/cache/results/kubenet-loopback-761f2c6dc1503077db58e85e19873758aae73d2fae4b920f9fe24674c1db920a-lo
	fname := getResultCacheFilePath(netName, rt)
	// 内容类似
	// {"cniVersion":"0.2.0","ip4":{"ip":"10.251.0.193/25","gateway":"10.251.0.129","routes":[{"dst":"0.0.0.0/0"}]},"dns":{}}
	// 后面的cni版本内容格式改成
	// {"kind":"cniCacheV1","containerId":"f89681f0fc8025fced6ef3d14e220086cff2a7e89876de387b41e7892b0428c5","config":"ewogICJjbmlWZXJzaW9uIjogIjAuMS4wIiwKICAibmFtZSI6ICJrdWJlbmV0IiwKICAidHlwZSI6ICJicmlkZ2UiLAogICJicmlkZ2UiOiAiY2JyMCIsCiAgIm10dSI6IDE1MDAsCiAgImFkZElmIjogImV0aDAiLAogICJpc0dhdGV3YXkiOiB0cnVlLAogICJpcE1hc3EiOiBmYWxzZSwKICAiaGFpcnBpbk1vZGUiOiB0cnVlLAogICJpcGFtIjogewogICAgInR5cGUiOiAiaG9zdC1sb2NhbCIsCiAgICAicmFuZ2VzIjogWwpbewoic3VibmV0IjogIjEwLjI0OC4wLjAvMjUiCn1dXSwKICAgICJyb3V0ZXMiOiBbeyJkc3QiOiAiMC4wLjAuMC8wIn1dCiAgfQp9","ifName":"eth0","networkName":"kubenet","result":{"cniVersion":"0.2.0","dns":{},"ip4":{"gateway":"10.248.0.1","ip":"10.248.0.88/25","routes":[{"dst":"0.0.0.0/0"}]}}}
	data, err := ioutil.ReadFile(fname)
	if err != nil {
		// Ignore read errors; the cached result may not exist on-disk
		return nil, nil
	}

	// Read the version of the cached result
	// 使用json.Unmarshal解析出cniVersion字段的值，如果没有这个字段或为空，则返回"0.1.0"
	decoder := version.ConfigDecoder{}
	resultCniVersion, err := decoder.Decode(data)
	if err != nil {
		return nil, err
	}

	// Ensure we can understand the result
	// 根据resultCniVersion使用相应版本NewResult来解析data
	result, err := version.NewResult(resultCniVersion, data)
	if err != nil {
		return nil, err
	}

	// Convert to the config version to ensure plugins get prevResult
	// in the same version as the config.  The cached result version
	// should match the config version unless the config was changed
	// while the container was running.
	// 转换为需要cniVersion版本的result
	result, err = result.GetAsVersion(cniVersion)
	if err != nil && resultCniVersion != cniVersion {
		return nil, fmt.Errorf("failed to convert cached result version %q to config version %q: %v", resultCniVersion, cniVersion, err)
	}
	return result, err
}

// GetNetworkListCachedResult returns the cached Result of the previous
// previous AddNetworkList() operation for a network list, or an error.
func (c *CNIConfig) GetNetworkListCachedResult(list *NetworkConfigList, rt *RuntimeConf) (types.Result, error) {
	return getCachedResult(list.Name, list.CNIVersion, rt)
}

// GetNetworkCachedResult returns the cached Result of the previous
// previous AddNetwork() operation for a network, or an error.
func (c *CNIConfig) GetNetworkCachedResult(net *NetworkConfig, rt *RuntimeConf) (types.Result, error) {
	return getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
}

func (c *CNIConfig) addNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) (types.Result, error) {
	// cniNetworkPlugin插件的loNetwork和defaultNetwork里的CNIConfig.exec都是nil
	// 这里如果c.exec为nil，则使用("github.com/containernetworking/cni/pkg/invoke")invoke.DefaultExec
	c.ensureExec()
	// 返回插件的绝对路径
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return nil, err
	}

	// 生成新的*NetworkConfig
	// 让cniVersion和name覆盖net.CNIVersion和net.name，prevResult（参数）赋值给newConf.RawPrevResult字段，newConf.Bytes字段里会有"prevResult": {prevResult参数转成[]byte}, 添加key为"runtimeConfig"，value为net.Network.Capabilities中启用的Capability的配置（在rt.CapabilityArgs中）到net.Bytes里
	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return nil, err
	}

	// cni loopback
	// 执行
	// echo '{"cniVersion": "0.2.0","name": "cni-loopback","type": "loopback"}' |  \
	// CNI_COMMAND="ADD" \
	// CNI_CONTAINERID="{container id}" \
	// CNI_NETNS="/proc/{container pid}/ns/net" \
	// CNI_ARGS="IgnoreUnknown=1;K8S_POD_NAMESPACE={NAMESPACE};K8S_POD_NAME={POD NAME};K8S_POD_INFRA_CONTAINER_ID={container id}" \
	// CNI_IFNAME="eth0" \  这个不是"lo"，这个在pkg\kubelet\dockershim\network\cni\cni.go里的buildCNIRuntimeConf写死了
	// CNI_PATH="/opt/cni/bin" \
	// /opt/cni/bin/loopback
	// 返回
	// {
	// "cniVersion": "0.2.0",
	// "ip4": {
	//     "ip": "127.0.0.1/8"
	//        },
	//  "dns": {}
	// }

	// kubenet-loopback
	// 执行
	// echo {"cniVersion": "0.1.0", "name": "kubenet-loopback", "type": "loopback"} | \
	// CNI_COMMAND="ADD" \
	// CNI_CONTAINERID="{container id}" \
	// CNI_NETNS="/proc/{container pid}/ns/net" \
	// CNI_ARGS="" \
	// CNI_IFNAME="lo" \
	// CNI_PATH="/opt/cni/bin" \
	// /opt/cni/bin/loopback
	// 返回
	// {
	// 	"cniVersion": "0.2.0",
	// 	"ip4": {
	// 		"ip": "127.0.0.1/8"
	// 	},
	// 	"dns": {}
	// }

	// kubenet
	// 执行
	// echo '{"cniVersion": "0.1.0", "name": "kubenet", "type": "bridge","bridge": "cbr0","mtu": 1500,"addIf": "eth0","isGateway": true,"ipMasq": false,"hairpinMode": true,"ipam": {"type": "host-local","ranges": [[{"subnet": "10.253.0.128/25"}]],"routes": [{"dst": "0.0.0.0/0"}]}}' | \
	// CNI_COMMAND="ADD" \
	// CNI_CONTAINERID="{container id}" \
	// CNI_NETNS="/proc/{container pid}/ns/net" \
	// CNI_ARGS="" \
	// CNI_IFNAME="lo" \
	// CNI_PATH="/opt/cni/bin" \
	// /usr/local/bin/bridge
	// 返回
	// {
	// 	"cniVersion": "0.2.0",
	// 	"ip4": {
	// 		"ip": "10.253.0.220/25",
	// 		"gateway": "10.253.0.129",
	// 		"routes": [
	// 			{
	// 				"dst": "0.0.0.0/0"
	// 			}
	// 		]
	// 	},
	// 	"dns": {}
	// }

	// 执行相应的插件，如果插件返回的cniVersion是支持的版本，则将插件执行输出（json格式）转成types.Result
	return invoke.ExecPluginWithResult(ctx, pluginPath, newConf.Bytes, c.args("ADD", rt), c.exec)
}

// AddNetworkList executes a sequence of plugins with the ADD command
func (c *CNIConfig) AddNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) (types.Result, error) {
	var err error
	var result types.Result
	for _, net := range list.Plugins {
		// 让list.CNIVersion和list.Name覆盖net.CNIVersion和net.name，上一个插件执行的result为新*NetworkConfig.RawPrevResult字段，同时会包含在新*NetworkConfig.Bytes字段（[]byte类型，类似为"preResult": {result转成[]byte}），添加key为"runtimeConfig"，value为net.Network.Capabilities中启用的Capability的配置（在rt.CapabilityArgs中）到net.Bytes里
		// 生成新的*NetworkConfig，然后新的*NetworkConfig.Bytes做为执行cni插件二进制文件的stdin
		// 然后调用c.args("ADD", rt)生成执行cni插件的环境变量（即把c.Path和rt转成环境变量），最后执行cni插件
		// cni插件执行的stdout输出会转成为result
		result, err = c.addNetwork(ctx, list.Name, list.CNIVersion, net, result, rt)
		if err != nil {
			return nil, err
		}
	}

	// 默认在/var/lib/cni/cache/results/下生成保存插件执行输出的文件
	if err = setCachedResult(result, list.Name, rt); err != nil {
		return nil, fmt.Errorf("failed to set network %q cached result: %v", list.Name, err)
	}

	return result, nil
}

func (c *CNIConfig) checkNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) error {
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return err
	}

	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return err
	}

	return invoke.ExecPluginWithoutResult(ctx, pluginPath, newConf.Bytes, c.args("CHECK", rt), c.exec)
}

// CheckNetworkList executes a sequence of plugins with the CHECK command
func (c *CNIConfig) CheckNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) error {
	// CHECK was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(list.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if !gtet {
		return fmt.Errorf("configuration version %q does not support the CHECK command", list.CNIVersion)
	}

	if list.DisableCheck {
		return nil
	}

	cachedResult, err := getCachedResult(list.Name, list.CNIVersion, rt)
	if err != nil {
		return fmt.Errorf("failed to get network %q cached result: %v", list.Name, err)
	}

	for _, net := range list.Plugins {
		if err := c.checkNetwork(ctx, list.Name, list.CNIVersion, net, cachedResult, rt); err != nil {
			return err
		}
	}

	return nil
}

// 标准输入为NetConf网络配置和设置相应环境变量来执行网络插件的"DEL"操作来释放ip
func (c *CNIConfig) delNetwork(ctx context.Context, name, cniVersion string, net *NetworkConfig, prevResult types.Result, rt *RuntimeConf) error {
	c.ensureExec()
	// 返回插件文件名为net.Network.Type的二进制文件位置
	pluginPath, err := c.exec.FindInPath(net.Network.Type, c.Path)
	if err != nil {
		return err
	}

	// 让cniVersion和name覆盖net.CNIVersion和net.name，prevResult赋值为net.RawPrevResult，net.Bytes里添加prevResult
	// 添加key为"runtimeConfig"，value为net.Network.Capabilities中启用的Capability的配置（在rt.CapabilityArgs中）到newConf.Bytes里
	newConf, err := buildOneConfig(name, cniVersion, net, prevResult, rt)
	if err != nil {
		return err
	}

	// 比如kubenet-loopback执行
	// echo {"cniVersion": "0.1.0", "name": "kubenet-loopback", "type": "loopback"} | \
	// CNI_COMMAND="DEL" \
	// CNI_CONTAINERID="{container id}" \
	// CNI_NETNS="/proc/{container pid}/ns/net" \
	// CNI_ARGS="" \
	// CNI_IFNAME="lo" \
	// CNI_PATH="/opt/cni/bin" \
	// /opt/cni/bin/loopback
	// 没有任何输出

	// kubenet
	// 执行
	// echo '{"cniVersion": "0.1.0", "name": "kubenet", "type": "bridge","bridge": "cbr0","mtu": 1500,"addIf": "eth0","isGateway": true,"ipMasq": false,"hairpinMode": true,"ipam": {"type": "host-local","ranges": [[{"subnet": "10.253.0.128/25"}]],"routes": [{"dst": "0.0.0.0/0"}]}, prevResult: {"cniVersion":"0.2.0","ip4":{"ip":"10.251.0.193/25","gateway":"10.251.0.129","routes":[{"dst":"0.0.0.0/0"}]},"dns":{}}}' | \
	// CNI_COMMAND="DEL" \
	// CNI_CONTAINERID="{container id}" \
	// CNI_NETNS="/proc/{container pid}/ns/net" \
	// CNI_ARGS="" \
	// CNI_IFNAME="eth0" \
	// CNI_PATH="/opt/cni/bin" \
	// /usr/local/bin/bridge
	// 没有任何输出

	// 执行命令忽略命令执行stderr输出
	return invoke.ExecPluginWithoutResult(ctx, pluginPath, newConf.Bytes, c.args("DEL", rt), c.exec)
}

// DelNetworkList executes a sequence of plugins with the DEL command
// 遍历所有list.Plugins插件，执行"DEL"操作来释放rt.IfName网卡和删除相关cni插件执行result缓存文件
func (c *CNIConfig) DelNetworkList(ctx context.Context, list *NetworkConfigList, rt *RuntimeConf) error {
	var cachedResult types.Result

	// Cached result on DEL was added in CNI spec version 0.4.0 and higher
	// 如果cni版本大于等于0.4.0，则从cni的缓存目录中获取cniVersion版本的result
	if gtet, err := version.GreaterThanOrEqualTo(list.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if gtet {
		// 从cni的缓存目录中获取cniVersion版本的result，如果文件不存在，则忽略这个错误，cachedResult为nil，err为nil
		cachedResult, err = getCachedResult(list.Name, list.CNIVersion, rt)
		if err != nil {
			return fmt.Errorf("failed to get network %q cached result: %v", list.Name, err)
		}
	}

	// 遍历所有插件，标准输入为网络配置和设置相应环境变量来执行网络插件的"DEL"操作来释放网卡
	for i := len(list.Plugins) - 1; i >= 0; i-- {
		net := list.Plugins[i]
		if err := c.delNetwork(ctx, list.Name, list.CNIVersion, net, cachedResult, rt); err != nil {
			return err
		}
	}
	// 删除{cacheDir}/results/{netName}/{container id}/{ifName}文件，容器相关的cache result结果缓存文件
	_ = delCachedResult(list.Name, rt)

	return nil
}

// AddNetwork executes the plugin with the ADD command
func (c *CNIConfig) AddNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) (types.Result, error) {
	// 根据net、rt生成新的*NetworkConfig，然后新的*NetworkConfig.Bytes做为执行cni插件二进制文件的stdin
	// 然后调用c.args("ADD", rt)生成执行cni插件的环境变量（即把c.Path和rt转成环境变量），最后执行cni插件
	// cni插件执行的stdout输出会转成为result
	result, err := c.addNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, nil, rt)
	if err != nil {
		return nil, err
	}

	// 默认在/var/lib/cni/cache/results/下生成保存插件执行输出的文件
	if err = setCachedResult(result, net.Network.Name, rt); err != nil {
		return nil, fmt.Errorf("failed to set network %q cached result: %v", net.Network.Name, err)
	}

	return result, nil
}

// CheckNetwork executes the plugin with the CHECK command
func (c *CNIConfig) CheckNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error {
	// CHECK was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(net.Network.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if !gtet {
		return fmt.Errorf("configuration version %q does not support the CHECK command", net.Network.CNIVersion)
	}

	cachedResult, err := getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
	if err != nil {
		return fmt.Errorf("failed to get network %q cached result: %v", net.Network.Name, err)
	}
	return c.checkNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, cachedResult, rt)
}

// DelNetwork executes the plugin with the DEL command
func (c *CNIConfig) DelNetwork(ctx context.Context, net *NetworkConfig, rt *RuntimeConf) error {
	var cachedResult types.Result

	// Cached result on DEL was added in CNI spec version 0.4.0 and higher
	if gtet, err := version.GreaterThanOrEqualTo(net.Network.CNIVersion, "0.4.0"); err != nil {
		return err
	} else if gtet {
		// 如果cni版本大于等于0.4.0，则从cni的缓存目录中获取cniVersion版本的result
		cachedResult, err = getCachedResult(net.Network.Name, net.Network.CNIVersion, rt)
		if err != nil {
			return fmt.Errorf("failed to get network %q cached result: %v", net.Network.Name, err)
		}
	}

	// kubenet插件（kubenet-loopback和kubenet）的cni版本都是0.1.0，所以这里的cachedResult为nil
	// 标准输入为网络配置和设置相应环境变量来执行网络插件的"DEL"操作来释放ip
	if err := c.delNetwork(ctx, net.Network.Name, net.Network.CNIVersion, net, cachedResult, rt); err != nil {
		return err
	}
	// 删除{cacheDir}/results/{netName}/{container id}/{ifName}文件，容器相关的cache result结果缓存文件
	_ = delCachedResult(net.Network.Name, rt)
	return nil
}

// ValidateNetworkList checks that a configuration is reasonably valid.
// - all the specified plugins exist on disk
// - every plugin supports the desired version.
//
// Returns a list of all capabilities supported by the configuration, or error
// 遍历所有plugins，验证plugin二进制文件是否存在，版本是否支持配置文件里指定的cniversion版本，返回plugin插件里的所有的Capability
func (c *CNIConfig) ValidateNetworkList(ctx context.Context, list *NetworkConfigList) ([]string, error) {
	version := list.CNIVersion

	// holding map for seen caps (in case of duplicates)
	caps := map[string]interface{}{}

	errs := []error{}
	// 遍历所有plugins，验证plugin二进制文件是否存在，版本是否支持配置文件里指定的cniversion版本，返回plugin插件里的所有的Capability
	for _, net := range list.Plugins {
		// 1. 检查插件的二进制文件是否存在
		// 2. version是否插件支持
		if err := c.validatePlugin(ctx, net.Network.Type, version); err != nil {
			errs = append(errs, err)
		}
		// 统计启用的capabilities
		for c, enabled := range net.Network.Capabilities {
			if !enabled {
				continue
			}
			caps[c] = struct{}{}
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%v", errs)
	}

	// make caps list
	cc := make([]string, 0, len(caps))
	for c := range caps {
		cc = append(cc, c)
	}

	return cc, nil
}

// ValidateNetwork checks that a configuration is reasonably valid.
// It uses the same logic as ValidateNetworkList)
// Returns a list of capabilities
func (c *CNIConfig) ValidateNetwork(ctx context.Context, net *NetworkConfig) ([]string, error) {
	caps := []string{}
	for c, ok := range net.Network.Capabilities {
		if ok {
			caps = append(caps, c)
		}
	}
	if err := c.validatePlugin(ctx, net.Network.Type, net.Network.CNIVersion); err != nil {
		return nil, err
	}
	return caps, nil
}

// validatePlugin checks that an individual plugin's configuration is sane
// 1. 检查插件的二进制文件是否存在
// 2. expectedVersion是否插件支持
func (c *CNIConfig) validatePlugin(ctx context.Context, pluginName, expectedVersion string) error {
	// 发现是插件的二进制文件存在
	pluginPath, err := invoke.FindInPath(pluginName, c.Path)
	if err != nil {
		return err
	}

	// 执行version动作获得插件支持的版本
	vi, err := invoke.GetVersionInfo(ctx, pluginPath, c.exec)
	if err != nil {
		return err
	}
	// expectedVersion是否匹配支持版本
	for _, vers := range vi.SupportedVersions() {
		if vers == expectedVersion {
			return nil
		}
	}
	return fmt.Errorf("plugin %s does not support config version %q", pluginName, expectedVersion)
}

// GetVersionInfo reports which versions of the CNI spec are supported by
// the given plugin.
func (c *CNIConfig) GetVersionInfo(ctx context.Context, pluginType string) (version.PluginInfo, error) {
	c.ensureExec()
	pluginPath, err := c.exec.FindInPath(pluginType, c.Path)
	if err != nil {
		return nil, err
	}

	return invoke.GetVersionInfo(ctx, pluginPath, c.exec)
}

// =====
// 忽略rt.CapabilityArgs和rt.CacheDir，只用了rt.ContainerID、rt.NetNS、rt.Args、rt.IfName
func (c *CNIConfig) args(action string, rt *RuntimeConf) *invoke.Args {
	return &invoke.Args{
		Command:     action,
		ContainerID: rt.ContainerID,
		NetNS:       rt.NetNS,
		PluginArgs:  rt.Args,
		IfName:      rt.IfName,
		Path:        strings.Join(c.Path, string(os.PathListSeparator)),
	}
}
