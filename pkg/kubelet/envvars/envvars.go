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

package envvars

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/api/core/v1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
)

// FromServices builds environment variables that a container is started with,
// which tell the container where to find the services it may need, which are
// provided as an argument.
// 生成service环境变量
func FromServices(services []*v1.Service) []v1.EnvVar {
	var result []v1.EnvVar
	for i := range services {
		service := services[i]

		// ignore services where ClusterIP is "None" or empty
		// the services passed to this method should be pre-filtered
		// only services that have the cluster IP set should be included here
		// 忽略没有分配cluster ip的service
		if !v1helper.IsServiceIPSet(service) {
			continue
		}

		// Host
		// 将service.Name里的横杠"-"替换成下划线"_"，然后转成大写，跟"_SERVICE_HOST"拼接
		// 比如"KUBERNETES_SERVICE_HOST"
		name := makeEnvVariableName(service.Name) + "_SERVICE_HOST"
		// value为service分配的cluster ip
		result = append(result, v1.EnvVar{Name: name, Value: service.Spec.ClusterIP})
		// First port - give it the backwards-compatible name
		// 比如"KUBERNETES_SERVICE_PORT"
		name = makeEnvVariableName(service.Name) + "_SERVICE_PORT"
		// value为service的第一个端口
		// 比如
		result = append(result, v1.EnvVar{Name: name, Value: strconv.Itoa(int(service.Spec.Ports[0].Port))})
		// All named ports (only the first may be unnamed, checked in validation)
		// 
		for i := range service.Spec.Ports {
			sp := &service.Spec.Ports[i]
			// service的port具有名字，则key为"{service name大写}_SERVICE_PORT_{port name大写}"，value为这个端口的值
			// 比如"KUBE_DNS_SERVICE_PORT_DNS"，value为53
			// 比如"KUBERNETES_SERVICE_PORT_HTTPS"，value为443
			if sp.Name != "" {
				pn := name + "_" + makeEnvVariableName(sp.Name)
				result = append(result, v1.EnvVar{Name: pn, Value: strconv.Itoa(int(sp.Port))})
			}
		}
		// Docker-compatible vars.
		// 生成docker service环境变量
		result = append(result, makeLinkVariables(service)...)
	}
	return result
}

// 将str里的横杠"-"替换成下划线"_"，然后转成大写
func makeEnvVariableName(str string) string {
	// TODO: If we simplify to "all names are DNS1123Subdomains" this
	// will need two tweaks:
	//   1) Handle leading digits
	//   2) Handle dots
	return strings.ToUpper(strings.Replace(str, "-", "_", -1))
}

// 生成docker service环境变量
func makeLinkVariables(service *v1.Service) []v1.EnvVar {
	prefix := makeEnvVariableName(service.Name)
	all := []v1.EnvVar{}
	for i := range service.Spec.Ports {
		sp := &service.Spec.Ports[i]

		protocol := string(v1.ProtocolTCP)
		if sp.Protocol != "" {
			protocol = string(sp.Protocol)
		}

		hostPort := net.JoinHostPort(service.Spec.ClusterIP, strconv.Itoa(int(sp.Port)))

		if i == 0 {
			// Docker special-cases the first port.
			// key为"{service name大写}_PORT"，value为"{protocol小写}://{cluster ip}:{port}"
			// 比如"KUBERNETES_PORT"，value为"tcp://10.0.0.1:443"
			all = append(all, v1.EnvVar{
				Name:  prefix + "_PORT",
				Value: fmt.Sprintf("%s://%s", strings.ToLower(protocol), hostPort),
			})
		}
		// 前缀为"{service name大写}_PORT_{port}_{protocol大写}"
		portPrefix := fmt.Sprintf("%s_PORT_%d_%s", prefix, sp.Port, strings.ToUpper(protocol))
		all = append(all, []v1.EnvVar{
			// key为"{service name大写}_PORT_{port}_{protocol大写}"，比如"KUBERNETES_PORT_443_TCP"
			// value为"{protocol小写}://{cluster ip}:{port}"，比如"tcp://10.0.0.1:443"
			{
				Name:  portPrefix,
				Value: fmt.Sprintf("%s://%s", strings.ToLower(protocol), hostPort),
			},
			// key为"{service name大写}_PORT_{port}_{protocol大写}_PROTO"，比如"KUBERNETES_PORT_443_TCP_PROTO"
			// value为"{protocol大写}"，比如"TCP"
			{
				Name:  portPrefix + "_PROTO",
				Value: strings.ToLower(protocol),
			},
			// key为"{service name大写}_PORT_{port}_{protocol大写}_PORT"，比如"KUBERNETES_PORT_443_TCP_PORT"
			// value为"{port}"
			{
				Name:  portPrefix + "_PORT",
				Value: strconv.Itoa(int(sp.Port)),
			},
			// key为"{service name大写}_PORT_{port}_{protocol大写}_ADDR"，比如"KUBERNETES_PORT_443_TCP_ADDR"
			// value为"{cluster ip}"
			{
				Name:  portPrefix + "_ADDR",
				Value: service.Spec.ClusterIP,
			},
		}...)
	}
	return all
}
