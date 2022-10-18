/*
Copyright 2016 The Kubernetes Authors.

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

package portforward

import (
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/wsstream"
)

// PortForwarder knows how to forward content from a data stream to/from a port
// in a pod.
type PortForwarder interface {
	// PortForwarder copies data between a data stream and a port in a pod.
	PortForward(name string, uid types.UID, port int32, stream io.ReadWriteCloser) error
}

// ServePortForward handles a port forwarding request.  A single request is
// kept alive as long as the client is still alive and the connection has not
// been timed out due to idleness. This function handles multiple forwarded
// connections; i.e., multiple `curl http://localhost:8888/` requests will be
// handled by a single invocation of ServePortForward.
func ServePortForward(w http.ResponseWriter, req *http.Request, portForwarder PortForwarder, podName string, uid types.UID, portForwardOptions *V4Options, idleTimeout time.Duration, streamCreationTimeout time.Duration, supportedProtocols []string) {
	var err error
	// header中"Upgrade"的值为"websocket"，且header中"Connection"匹配"(^|.*,\\s*)upgrade($|\\s*,)"，则返回true
	if wsstream.IsWebSocketRequest(req) {
		// 启用一个goroutine来处理websocket请求
		// 请求里的每个port Forward，启动一个goroutine
		// 查询容器是否存在
		// 容器不在运行状态，直接返回错误
		// 从streamPair.dataStream中读取数据作为stdin，发送stdout数据到streamPair.dataStream
		// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
		// 如果发生错误，则发送错误到streamPair.errorStream
		err = handleWebSocketStreams(req, w, portForwarder, podName, uid, portForwardOptions, supportedProtocols, idleTimeout, streamCreationTimeout)
	} else {
		// 校验请求头部"X-Stream-Protocol-Version"，判断是否支持
		// 校验请求header中"Connection"的值是否为"Upgrade"，header中"Upgrade"的值是否为"SPDY/3.1"
		// 创建一个goroutine来服务stream frames
		// 启动一个goroutine处理stream中dataStream和errorStream
		// 从dataStream中读取数据作为stdin，发送stdout数据到stream
		// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
		// 有任何错误，发送到errorStream
		err = handleHTTPStreams(req, w, portForwarder, podName, uid, supportedProtocols, idleTimeout, streamCreationTimeout)
	}

	if err != nil {
		runtime.HandleError(err)
		return
	}
}
