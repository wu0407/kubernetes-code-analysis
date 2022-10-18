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

package remotecommand

import (
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/apiserver/pkg/util/wsstream"
)

const (
	stdinChannel = iota
	stdoutChannel
	stderrChannel
	errorChannel
	resizeChannel

	preV4BinaryWebsocketProtocol = wsstream.ChannelWebSocketProtocol
	preV4Base64WebsocketProtocol = wsstream.Base64ChannelWebSocketProtocol
	v4BinaryWebsocketProtocol    = "v4." + wsstream.ChannelWebSocketProtocol
	v4Base64WebsocketProtocol    = "v4." + wsstream.Base64ChannelWebSocketProtocol
)

// createChannels returns the standard channel types for a shell connection (STDIN 0, STDOUT 1, STDERR 2)
// along with the approximate duplex value. It also creates the error (3) and resize (4) channels.
// 根据Options，创建出5个wsstream.ChannelType
// 分别代表STDIN 0, STDOUT 1, STDERR 2，error (3) and resize (4)
func createChannels(opts *Options) []wsstream.ChannelType {
	// open the requested channels, and always open the error channel
	channels := make([]wsstream.ChannelType, 5)
	channels[stdinChannel] = readChannel(opts.Stdin)
	channels[stdoutChannel] = writeChannel(opts.Stdout)
	channels[stderrChannel] = writeChannel(opts.Stderr)
	channels[errorChannel] = wsstream.WriteChannel
	channels[resizeChannel] = wsstream.ReadChannel
	return channels
}

// readChannel returns wsstream.ReadChannel if real is true, or wsstream.IgnoreChannel.
func readChannel(real bool) wsstream.ChannelType {
	if real {
		return wsstream.ReadChannel
	}
	return wsstream.IgnoreChannel
}

// writeChannel returns wsstream.WriteChannel if real is true, or wsstream.IgnoreChannel.
func writeChannel(real bool) wsstream.ChannelType {
	if real {
		return wsstream.WriteChannel
	}
	return wsstream.IgnoreChannel
}

// createWebSocketStreams returns a context containing the websocket connection and
// streams needed to perform an exec or an attach.
// 创建websocket connection和streams，用来执行exec和attach
func createWebSocketStreams(req *http.Request, w http.ResponseWriter, opts *Options, idleTimeout time.Duration) (*context, bool) {
	// 根据Options，创建出5个wsstream.ChannelType
	// 分别代表STDIN 0, STDOUT 1, STDERR 2，error (3) and resize (4)
	channels := createChannels(opts)
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		"": {
			Binary:   true,
			Channels: channels,
		},
		// "channel.k8s.io"
		preV4BinaryWebsocketProtocol: {
			Binary:   true,
			Channels: channels,
		},
		// "base64.channel.k8s.io"
		preV4Base64WebsocketProtocol: {
			Binary:   false,
			Channels: channels,
		},
		// "v4.channel.k8s.io"
		v4BinaryWebsocketProtocol: {
			Binary:   true,
			Channels: channels,
		},
		// "v4.base64.channel.k8s.io"
		v4Base64WebsocketProtocol: {
			Binary:   false,
			Channels: channels,
		},
	})
	// 设置conn.timeout为idleTimeout
	conn.SetIdleTimeout(idleTimeout)
	// httplog.Unlogged
	// req中context里如果保存了respLogger，则返回respLogger.w，否则返回w
	//
	// 启用一个goroutine来处理websocket请求
	// 返回所有fd对应的websocketChannel
	negotiatedProtocol, streams, err := conn.Open(httplog.Unlogged(req, w), req)
	if err != nil {
		runtime.HandleError(fmt.Errorf("unable to upgrade websocket connection: %v", err))
		return nil, false
	}

	// Send an empty message to the lowest writable channel to notify the client the connection is established
	// TODO: make generic to SPDY and WebSockets and do it outside of this method?
	// 发送空消息给客户端，通知链接已经建立
	switch {
	case opts.Stdout:
		streams[stdoutChannel].Write([]byte{})
	case opts.Stderr:
		streams[stderrChannel].Write([]byte{})
	default:
		streams[errorChannel].Write([]byte{})
	}

	ctx := &context{
		conn:         conn,
		stdinStream:  streams[stdinChannel],
		stdoutStream: streams[stdoutChannel],
		stderrStream: streams[stderrChannel],
		tty:          opts.TTY,
		resizeStream: streams[resizeChannel],
	}

	switch negotiatedProtocol {
	// 协议类型是"v4.channel.k8s.io"或"v4.base64.channel.k8s.io"
	case v4BinaryWebsocketProtocol, v4Base64WebsocketProtocol:
		ctx.writeStatus = v4WriteStatusFunc(streams[errorChannel])
	default:
		ctx.writeStatus = v1WriteStatusFunc(streams[errorChannel])
	}

	return ctx, true
}
