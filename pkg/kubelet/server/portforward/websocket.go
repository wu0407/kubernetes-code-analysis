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
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/apiserver/pkg/util/wsstream"
	api "k8s.io/kubernetes/pkg/apis/core"
)

const (
	dataChannel = iota
	errorChannel

	v4BinaryWebsocketProtocol = "v4." + wsstream.ChannelWebSocketProtocol
	v4Base64WebsocketProtocol = "v4." + wsstream.Base64ChannelWebSocketProtocol
)

// V4Options contains details about which streams are required for port
// forwarding.
// All fields included in V4Options need to be expressed explicitly in the
// CRI (k8s.io/cri-api/pkg/apis/{version}/api.proto) PortForwardRequest.
type V4Options struct {
	Ports []int32
}

// NewV4Options creates a new options from the Request.
func NewV4Options(req *http.Request) (*V4Options, error) {
	// header中"Upgrade"的值不为"websocket"，或header中"Connection"不匹配"(^|.*,\\s*)upgrade($|\\s*,)
	if !wsstream.IsWebSocketRequest(req) {
		return &V4Options{}, nil
	}

	// 获得url query里的"port"值
	portStrings := req.URL.Query()[api.PortHeader]
	if len(portStrings) == 0 {
		return nil, fmt.Errorf("query parameter %q is required", api.PortHeader)
	}

	ports := make([]int32, 0, len(portStrings))
	for _, portString := range portStrings {
		if len(portString) == 0 {
			return nil, fmt.Errorf("query parameter %q cannot be empty", api.PortHeader)
		}
		for _, p := range strings.Split(portString, ",") {
			port, err := strconv.ParseUint(p, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("unable to parse %q as a port: %v", portString, err)
			}
			if port < 1 {
				return nil, fmt.Errorf("port %q must be > 0", portString)
			}
			ports = append(ports, int32(port))
		}
	}

	return &V4Options{
		Ports: ports,
	}, nil
}

// BuildV4Options returns a V4Options based on the given information.
func BuildV4Options(ports []int32) (*V4Options, error) {
	return &V4Options{Ports: ports}, nil
}

// handleWebSocketStreams handles requests to forward ports to a pod via
// a PortForwarder. A pair of streams are created per port (DATA n,
// ERROR n+1). The associated port is written to each stream as a unsigned 16
// bit integer in little endian format.
// 启用一个goroutine来处理websocket请求
// 请求里的每个port Forward，启动一个goroutine
// 查询容器是否存在
// 容器不在运行状态，直接返回错误
// 从streamPair.dataStream中读取数据作为stdin，发送stdout数据到streamPair.dataStream
// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
// 如果发生错误，则发送错误到streamPair.errorStream
func handleWebSocketStreams(req *http.Request, w http.ResponseWriter, portForwarder PortForwarder, podName string, uid types.UID, opts *V4Options, supportedPortForwardProtocols []string, idleTimeout, streamCreationTimeout time.Duration) error {
	channels := make([]wsstream.ChannelType, 0, len(opts.Ports)*2)
	for i := 0; i < len(opts.Ports); i++ {
		// 一个端口包含了data wsstream.ReadWriteChannel和error wsstream.WriteChannel
		channels = append(channels, wsstream.ReadWriteChannel, wsstream.WriteChannel)
	}
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		"": {
			Binary:   true,
			Channels: channels,
		},
		v4BinaryWebsocketProtocol: {
			Binary:   true,
			Channels: channels,
		},
		v4Base64WebsocketProtocol: {
			Binary:   false,
			Channels: channels,
		},
	})
	// 设置conn.timeout为idleTimeout
	conn.SetIdleTimeout(idleTimeout)
	// 启用一个goroutine来处理websocket请求
	// 返回所有channel类型对应的websocketChannel
	_, streams, err := conn.Open(httplog.Unlogged(req, w), req)
	if err != nil {
		err = fmt.Errorf("unable to upgrade websocket connection: %v", err)
		return err
	}
	defer conn.Close()
	streamPairs := make([]*websocketStreamPair, len(opts.Ports))
	for i := range streamPairs {
		streamPair := websocketStreamPair{
			port:        opts.Ports[i],
			// streams[0]、streams[2]、streams[i*2]...
			dataStream:  streams[i*2+dataChannel],
			// streams[1]、stream[3]、streams[i*2+1]...
			errorStream: streams[i*2+errorChannel],
		}
		streamPairs[i] = &streamPair

		portBytes := make([]byte, 2)
		// port is always positive so conversion is allowable
		binary.LittleEndian.PutUint16(portBytes, uint16(streamPair.port))
		// 端口号写入数据流
		streamPair.dataStream.Write(portBytes)
		// 端口号写入错误流
		streamPair.errorStream.Write(portBytes)
	}
	h := &websocketStreamHandler{
		conn:        conn,
		streamPairs: streamPairs,
		pod:         podName,
		uid:         uid,
		// portForwarder实现在pkg\kubelet\server\streaming\server.go里的criAdapter
		forwarder:   portForwarder,
	}
	// 每个port Forward，启动一个goroutine
	// 查询容器是否存在
	// 容器不在运行状态，直接返回错误
	// 从streamPair.dataStream中读取数据作为stdin，发送stdout数据到streamPair.dataStream
	// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
	// 如果发生错误，则发送错误到streamPair.errorStream
	h.run()

	return nil
}

// websocketStreamPair represents the error and data streams for a port
// forwarding request.
type websocketStreamPair struct {
	port        int32
	dataStream  io.ReadWriteCloser
	errorStream io.WriteCloser
}

// websocketStreamHandler is capable of processing a single port forward
// request over a websocket connection
type websocketStreamHandler struct {
	conn        *wsstream.Conn
	streamPairs []*websocketStreamPair
	pod         string
	uid         types.UID
	forwarder   PortForwarder
}

// run invokes the websocketStreamHandler's forwarder.PortForward
// function for the given stream pair.
// 每个port Forward，启动一个goroutine
// 查询容器是否存在
// 容器不在运行状态，直接返回错误
// 从p.dataStream中读取数据作为stdin，发送stdout数据到p.dataStream
// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
// 如果发生错误，则发送错误到streamPair.errorStream
func (h *websocketStreamHandler) run() {
	wg := sync.WaitGroup{}
	wg.Add(len(h.streamPairs))

	for _, pair := range h.streamPairs {
		p := pair
		// 启动一个goroutine
		// 查询容器是否存在
		// 容器不在运行状态，直接返回错误
		// 从p.dataStream中读取数据作为stdin，发送stdout数据到p.dataStream
		// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
		// 如果发生错误，则发送错误到p.errorStream
		go func() {
			defer wg.Done()
			h.portForward(p)
		}()
	}

	wg.Wait()
}

// 查询容器是否存在
// 容器不在运行状态，直接返回错误
// 从p.dataStream中读取数据作为stdin，发送stdout数据到p.dataStream
// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
// 如果发生错误，则发送错误到p.errorStream
func (h *websocketStreamHandler) portForward(p *websocketStreamPair) {
	defer p.dataStream.Close()
	defer p.errorStream.Close()

	klog.V(5).Infof("(conn=%p) invoking forwarder.PortForward for port %d", h.conn, p.port)
	// portForwarder实现在pkg\kubelet\server\streaming\server.go里的criAdapter
	// 查询容器是否存在
	// 容器不在运行状态，直接返回错误
	// 从p.dataStream中读取数据作为stdin，发送stdout数据到p.dataStream
	// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
	err := h.forwarder.PortForward(h.pod, h.uid, p.port, p.dataStream)
	klog.V(5).Infof("(conn=%p) done invoking forwarder.PortForward for port %d", h.conn, p.port)

	// 如果发生错误，则发送错误到p.errorStream
	if err != nil {
		msg := fmt.Errorf("error forwarding port %d to pod %s, uid %v: %v", p.port, h.pod, h.uid, err)
		runtime.HandleError(msg)
		fmt.Fprint(p.errorStream, msg.Error())
	}
}
