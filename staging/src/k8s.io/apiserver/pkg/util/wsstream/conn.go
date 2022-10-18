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

package wsstream

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/websocket"
	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/util/runtime"
)

// The Websocket subprotocol "channel.k8s.io" prepends each binary message with a byte indicating
// the channel number (zero indexed) the message was sent on. Messages in both directions should
// prefix their messages with this channel byte. When used for remote execution, the channel numbers
// are by convention defined to match the POSIX file-descriptors assigned to STDIN, STDOUT, and STDERR
// (0, 1, and 2). No other conversion is performed on the raw subprotocol - writes are sent as they
// are received by the server.
//
// Example client session:
//
//    CONNECT http://server.com with subprotocol "channel.k8s.io"
//    WRITE []byte{0, 102, 111, 111, 10} # send "foo\n" on channel 0 (STDIN)
//    READ  []byte{1, 10}                # receive "\n" on channel 1 (STDOUT)
//    CLOSE
//
const ChannelWebSocketProtocol = "channel.k8s.io"

// The Websocket subprotocol "base64.channel.k8s.io" base64 encodes each message with a character
// indicating the channel number (zero indexed) the message was sent on. Messages in both directions
// should prefix their messages with this channel char. When used for remote execution, the channel
// numbers are by convention defined to match the POSIX file-descriptors assigned to STDIN, STDOUT,
// and STDERR ('0', '1', and '2'). The data received on the server is base64 decoded (and must be
// be valid) and data written by the server to the client is base64 encoded.
//
// Example client session:
//
//    CONNECT http://server.com with subprotocol "base64.channel.k8s.io"
//    WRITE []byte{48, 90, 109, 57, 118, 67, 103, 111, 61} # send "foo\n" (base64: "Zm9vCgo=") on channel '0' (STDIN)
//    READ  []byte{49, 67, 103, 61, 61} # receive "\n" (base64: "Cg==") on channel '1' (STDOUT)
//    CLOSE
//
const Base64ChannelWebSocketProtocol = "base64.channel.k8s.io"

type codecType int

const (
	rawCodec codecType = iota
	base64Codec
)

type ChannelType int

const (
	IgnoreChannel ChannelType = iota
	ReadChannel
	WriteChannel
	ReadWriteChannel
)

var (
	// connectionUpgradeRegex matches any Connection header value that includes upgrade
	connectionUpgradeRegex = regexp.MustCompile("(^|.*,\\s*)upgrade($|\\s*,)")
)

// IsWebSocketRequest returns true if the incoming request contains connection upgrade headers
// for WebSockets.
// header中"Upgrade"的值为"websocket"，且header中"Connection"匹配"(^|.*,\\s*)upgrade($|\\s*,)"，则返回true
func IsWebSocketRequest(req *http.Request) bool {
	// header中"Upgrade"的值不为"websocket"
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return false
	}
	// header中"Connection"，是否匹配"(^|.*,\\s*)upgrade($|\\s*,)"
	return connectionUpgradeRegex.MatchString(strings.ToLower(req.Header.Get("Connection")))
}

// IgnoreReceives reads from a WebSocket until it is closed, then returns. If timeout is set, the
// read and write deadlines are pushed every time a new message is received.
func IgnoreReceives(ws *websocket.Conn, timeout time.Duration) {
	defer runtime.HandleCrash()
	var data []byte
	for {
		resetTimeout(ws, timeout)
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return
		}
	}
}

// handshake ensures the provided user protocol matches one of the allowed protocols. It returns
// no error if no protocol is specified.
// 修改config.protocol为config.protocol里第一个支持的协议
// 如果config.Protoco为空，则config.protocol为空协议
func handshake(config *websocket.Config, req *http.Request, allowed []string) error {
	protocols := config.Protocol
	if len(protocols) == 0 {
		protocols = []string{""}
	}

	// 修改config.protocol为config.protocol里第一个支持的协议
	for _, protocol := range protocols {
		for _, allow := range allowed {
			if allow == protocol {
				config.Protocol = []string{protocol}
				return nil
			}
		}
	}

	return fmt.Errorf("requested protocol(s) are not supported: %v; supports %v", config.Protocol, allowed)
}

// ChannelProtocolConfig describes a websocket subprotocol with channels.
type ChannelProtocolConfig struct {
	Binary   bool
	Channels []ChannelType
}

// NewDefaultChannelProtocols returns a channel protocol map with the
// subprotocols "", "channel.k8s.io", "base64.channel.k8s.io" and the given
// channels.
func NewDefaultChannelProtocols(channels []ChannelType) map[string]ChannelProtocolConfig {
	return map[string]ChannelProtocolConfig{
		"":                             {Binary: true, Channels: channels},
		ChannelWebSocketProtocol:       {Binary: true, Channels: channels},
		Base64ChannelWebSocketProtocol: {Binary: false, Channels: channels},
	}
}

// Conn supports sending multiple binary channels over a websocket connection.
type Conn struct {
	// 服务端支持的协议名字与对应协议配置（数据格式、channel类型（读写、只读、只写））
	protocols        map[string]ChannelProtocolConfig
	selectedProtocol string
	// exec和attach是每个fd（0为stdin，1为stdout，2为stderr，3为error，4为resize）对应的websocketChannel
	// portforward是每个端口data wsstream.ReadWriteChannel和error wsstream.WriteChannel
	channels         []*websocketChannel
	codec            codecType
	ready            chan struct{}
	ws               *websocket.Conn
	timeout          time.Duration
}

// NewConn creates a WebSocket connection that supports a set of channels. Channels begin each
// web socket message with a single byte indicating the channel number (0-N). 255 is reserved for
// future use. The channel types for each channel are passed as an array, supporting the different
// duplex modes. Read and Write refer to whether the channel can be used as a Reader or Writer.
//
// The protocols parameter maps subprotocol names to ChannelProtocols. The empty string subprotocol
// name is used if websocket.Config.Protocol is empty.
func NewConn(protocols map[string]ChannelProtocolConfig) *Conn {
	return &Conn{
		ready:     make(chan struct{}),
		protocols: protocols,
	}
}

// SetIdleTimeout sets the interval for both reads and writes before timeout. If not specified,
// there is no timeout on the connection.
func (conn *Conn) SetIdleTimeout(duration time.Duration) {
	conn.timeout = duration
}

// Open the connection and create channels for reading and writing. It returns
// the selected subprotocol, a slice of channels and an error.
// 启用一个goroutine来处理websocket请求
// 返回所有channel类型（exec和attach对应的fd号）对应的websocketChannel
func (conn *Conn) Open(w http.ResponseWriter, req *http.Request) (string, []io.ReadWriteCloser, error) {
	// 启用一个goroutine来处理websocket请求
	go func() {
		defer runtime.HandleCrash()
		defer conn.Close()
		websocket.Server{Handshake: conn.handshake, Handler: conn.handle}.ServeHTTP(w, req)
	}()
	<-conn.ready
	rwc := make([]io.ReadWriteCloser, len(conn.channels))
	for i := range conn.channels {
		rwc[i] = conn.channels[i]
	}
	return conn.selectedProtocol, rwc, nil
}

// 设置conn.selectedProtocol，conn.codec，conn.ws，conn.channels
// 关闭conn.ready
func (conn *Conn) initialize(ws *websocket.Conn) {
	negotiated := ws.Config().Protocol
	conn.selectedProtocol = negotiated[0]
	// 协议配置
	p := conn.protocols[conn.selectedProtocol]
	if p.Binary {
		conn.codec = rawCodec
	} else {
		conn.codec = base64Codec
	}
	conn.ws = ws
	conn.channels = make([]*websocketChannel, len(p.Channels))
	for i, t := range p.Channels {
		switch t {
		case ReadChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), true, false)
		case WriteChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), false, true)
		case ReadWriteChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), true, true)
		case IgnoreChannel:
			conn.channels[i] = newWebsocketChannel(conn, byte(i), false, false)
		}
	}

	close(conn.ready)
}

func (conn *Conn) handshake(config *websocket.Config, req *http.Request) error {
	supportedProtocols := make([]string, 0, len(conn.protocols))
	// 遍历所有支持的协议
	for p := range conn.protocols {
		supportedProtocols = append(supportedProtocols, p)
	}
	// 修改config.protocol为config.protocol里第一个支持的协议
	// 如果config.Protocol为空，则修改config.protocol为空协议
	return handshake(config, req, supportedProtocols)
}

// 设置conn.ws的deadline
func (conn *Conn) resetTimeout() {
	if conn.timeout > 0 {
		conn.ws.SetDeadline(time.Now().Add(conn.timeout))
	}
}

// Close is only valid after Open has been called
func (conn *Conn) Close() error {
	<-conn.ready
	for _, s := range conn.channels {
		s.Close()
	}
	conn.ws.Close()
	return nil
}

// handle implements a websocket handler.
func (conn *Conn) handle(ws *websocket.Conn) {
	defer conn.Close()
	// 设置conn.selectedProtocol，conn.codec，conn.ws，conn.channels
	// 关闭conn.ready
	conn.initialize(ws)

	for {
		// 设置conn.ws的deadline
		conn.resetTimeout()
		var data []byte
		// 接受websocket消息
		if err := websocket.Message.Receive(ws, &data); err != nil {
			if err != io.EOF {
				klog.Errorf("Error on socket receive: %v", err)
			}
			break
		}
		if len(data) == 0 {
			continue
		}
		// 第一个字节，代表通道号（fd号）
		channel := data[0]
		// base64解码，则channel需要减去'0'的字节码
		if conn.codec == base64Codec {
			channel = channel - '0'
		}
		data = data[1:]
		if int(channel) >= len(conn.channels) {
			klog.V(6).Infof("Frame is targeted for a reader %d that is not valid, possible protocol error", channel)
			continue
		}
		// 如果websocketChannel需要读数据，则根据解码器类型，采取不同动作
		// 如果是rawCodec，则直接将data写入到pipe中，等待数据被消费
		// 如果是base64Codec，则对数据进行base64解码，然后将解码后的data写入到pipe中，等待数据被消费
		if _, err := conn.channels[channel].DataFromSocket(data); err != nil {
			klog.Errorf("Unable to write frame to %d: %v\n%s", channel, err, string(data))
			continue
		}
	}
}

// write multiplexes the specified channel onto the websocket
func (conn *Conn) write(num byte, data []byte) (int, error) {
	// 设置conn.ws的deadline
	conn.resetTimeout()
	switch conn.codec {
	case rawCodec:
		frame := make([]byte, len(data)+1)
		// 第一字节为fd号（协议号）
		frame[0] = num
		copy(frame[1:], data)
		if err := websocket.Message.Send(conn.ws, frame); err != nil {
			return 0, err
		}
	case base64Codec:
		// 第一字节是fd号加'0'的字节码
		frame := string('0'+num) + base64.StdEncoding.EncodeToString(data)
		if err := websocket.Message.Send(conn.ws, frame); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

// websocketChannel represents a channel in a connection
type websocketChannel struct {
	conn *Conn
	num  byte
	r    io.Reader
	w    io.WriteCloser

	read, write bool
}

// newWebsocketChannel creates a pipe for writing to a websocket. Do not write to this pipe
// prior to the connection being opened. It may be no, half, or full duplex depending on
// read and write.
func newWebsocketChannel(conn *Conn, num byte, read, write bool) *websocketChannel {
	r, w := io.Pipe()
	return &websocketChannel{conn, num, r, w, read, write}
}

// 将数据写入到websocket的conn里
func (p *websocketChannel) Write(data []byte) (int, error) {
	// 如果这个websocketChannel不需要写，则直接返回data大小和nil
	if !p.write {
		return len(data), nil
	}
	return p.conn.write(p.num, data)
}

// DataFromSocket is invoked by the connection receiver to move data from the connection
// into a specific channel.
// 如果websocketChannel需要读数据，则根据解码器类型，采取不同动作
// 如果是rawCodec，则直接将data写入到pipe中，等待数据被消费
// 如果是base64Codec，则对数据进行base64解码，然后将解码后的data写入到pipe中，等待数据被消费
func (p *websocketChannel) DataFromSocket(data []byte) (int, error) {
	// 如果这个websocketChannel不需要读，则直接返回data大小和nil
	if !p.read {
		return len(data), nil
	}

	switch p.conn.codec {
	case rawCodec:
		// 写入到pipe中，等待数据被消费
		return p.w.Write(data)
	case base64Codec:
		dst := make([]byte, len(data))
		// 对数据进行base64解码
		n, err := base64.StdEncoding.Decode(dst, data)
		if err != nil {
			return 0, err
		}
		// 写入到pipe中，等待数据被消费
		return p.w.Write(dst[:n])
	}
	return 0, nil
}

// 这个应该在docker中被执行，比如stdin读入在p.DataFromSocket里客户端发送写入到p.w
func (p *websocketChannel) Read(data []byte) (int, error) {
	if !p.read {
		return 0, io.EOF
	}
	return p.r.Read(data)
}

func (p *websocketChannel) Close() error {
	return p.w.Close()
}
