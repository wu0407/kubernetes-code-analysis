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
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/klog"
)

// 校验请求头部"X-Stream-Protocol-Version"，判断是否支持
// 校验请求header中"Connection"的值是否为"Upgrade"，header中"Upgrade"的值是否为"SPDY/3.1"
// 创建一个goroutine来服务stream frames
// 启动一个goroutine处理stream中dataStream和errorStream
// 从dataStream中读取数据作为stdin，发送stdout数据到stream
// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
// 有任何错误，发送到errorStream
func handleHTTPStreams(req *http.Request, w http.ResponseWriter, portForwarder PortForwarder, podName string, uid types.UID, supportedPortForwardProtocols []string, idleTimeout, streamCreationTimeout time.Duration) error {
	// 从请求的头部"X-Stream-Protocol-Version"值，找到第一个在serverProtocols中的protocol
	// 如果找不到，则响应header中添加"X-Accepted-Stream-Protocol-Versions" header头部，值为服务端支持的protocol，http code为403
	// 否则响应header中添加key为"X-Stream-Protocol-Version"，值为第一个支持的版本
	_, err := httpstream.Handshake(req, w, supportedPortForwardProtocols)
	// negotiated protocol isn't currently used server side, but could be in the future
	if err != nil {
		// Handshake writes the error to the client
		return err
	}
	streamChan := make(chan httpstream.Stream, 1)

	klog.V(5).Infof("Upgrading port forward response")
	upgrader := spdy.NewResponseUpgrader()
	// httpStreamReceived
	// 校验stream header里获得"port"的值，不能为空，必须是大于0的数字
	// 校验stream header里获得"streamType"的值，不能为空，必须是"error"或"data"
	// 发送spdystream到streams
	//
	// 校验请求header中"Connection"的值是否为"Upgrade"，header中"Upgrade"的值是否为"SPDY/3.1"
	// 如果请求合法，则响应的http header添加key为"Connection"，value为"Upgrade"和key为"Upgrade"，value为"SPDY/3.1"和http code为101
	// 创建一个goroutine来服务stream frames
	// 返回包装了spdystream.Connection的connection
	conn := upgrader.UpgradeResponse(w, req, httpStreamReceived(streamChan))
	if conn == nil {
		return errors.New("unable to upgrade httpstream connection")
	}
	defer conn.Close()

	klog.V(5).Infof("(conn=%p) setting port forwarding streaming connection idle timeout to %v", conn, idleTimeout)
	conn.SetIdleTimeout(idleTimeout)

	h := &httpStreamHandler{
		conn:                  conn,
		streamChan:            streamChan,
		streamPairs:           make(map[string]*httpStreamPair),
		streamCreationTimeout: streamCreationTimeout,
		pod:                   podName,
		uid:                   uid,
		forwarder:             portForwarder,
	}
	// 处理stream中dataStream和errorStream
	// 从dataStream中读取数据作为stdin，发送stdout数据到stream
	// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
	// 有任何错误，发送到errorStream
	h.run()

	return nil
}

// httpStreamReceived is the httpstream.NewStreamHandler for port
// forward streams. It checks each stream's port and stream type headers,
// rejecting any streams that with missing or invalid values. Each valid
// stream is sent to the streams channel.
// 校验stream header里获得"port"的值，不能为空，必须是大于0的数字
// 校验stream header里获得"streamType"的值，不能为空，必须是"error"或"data"
// 发送spdystream到streams
func httpStreamReceived(streams chan httpstream.Stream) func(httpstream.Stream, <-chan struct{}) error {
	return func(stream httpstream.Stream, replySent <-chan struct{}) error {
		// make sure it has a valid port header
		// 从stream header里获得"port"的值
		portString := stream.Headers().Get(api.PortHeader)
		if len(portString) == 0 {
			return fmt.Errorf("%q header is required", api.PortHeader)
		}
		port, err := strconv.ParseUint(portString, 10, 16)
		if err != nil {
			return fmt.Errorf("unable to parse %q as a port: %v", portString, err)
		}
		if port < 1 {
			return fmt.Errorf("port %q must be > 0", portString)
		}

		// make sure it has a valid stream type header
		// 从stream header里获得"streamType"的值
		streamType := stream.Headers().Get(api.StreamType)
		if len(streamType) == 0 {
			return fmt.Errorf("%q header is required", api.StreamType)
		}
		// "streamType"的值不是"error"，且不是"data"
		if streamType != api.StreamTypeError && streamType != api.StreamTypeData {
			return fmt.Errorf("invalid stream type %q", streamType)
		}

		streams <- stream
		return nil
	}
}

// httpStreamHandler is capable of processing multiple port forward
// requests over a single httpstream.Connection.
type httpStreamHandler struct {
	conn                  httpstream.Connection
	streamChan            chan httpstream.Stream
	streamPairsLock       sync.RWMutex
	streamPairs           map[string]*httpStreamPair
	streamCreationTimeout time.Duration
	pod                   string
	uid                   types.UID
	forwarder             PortForwarder
}

// getStreamPair returns a httpStreamPair for requestID. This creates a
// new pair if one does not yet exist for the requestID. The returned bool is
// true if the pair was created.
// 从h.streamPairs获得requestID对应的httpStreamPair
// 如果存在，则返回已经存在的httpStreamPair和false
// 如果不存在，则创建新的httpStreamPair，保存到h.streamPairs，返回创建的httpStreamPair和true
func (h *httpStreamHandler) getStreamPair(requestID string) (*httpStreamPair, bool) {
	h.streamPairsLock.Lock()
	defer h.streamPairsLock.Unlock()

	// 如果requestID已经存在h.streamPairs，则返回已经存在的httpStreamPair
	if p, ok := h.streamPairs[requestID]; ok {
		klog.V(5).Infof("(conn=%p, request=%s) found existing stream pair", h.conn, requestID)
		return p, false
	}

	klog.V(5).Infof("(conn=%p, request=%s) creating new stream pair", h.conn, requestID)

	// 创建新的httpStreamPair
	p := newPortForwardPair(requestID)
	// 保存到h.streamPairs
	h.streamPairs[requestID] = p

	return p, true
}

// monitorStreamPair waits for the pair to receive both its error and data
// streams, or for the timeout to expire (whichever happens first), and then
// removes the pair.
// 等待timeout超时，或p.complete消息（已经收到了error和data stream，在p.add中关闭p.complete），然后从h.streamPairs移除p.requestID
func (h *httpStreamHandler) monitorStreamPair(p *httpStreamPair, timeout <-chan time.Time) {
	select {
	case <-timeout:
		err := fmt.Errorf("(conn=%v, request=%s) timed out waiting for streams", h.conn, p.requestID)
		utilruntime.HandleError(err)
		p.printError(err.Error())
	case <-p.complete:
		klog.V(5).Infof("(conn=%v, request=%s) successfully received error and data streams", h.conn, p.requestID)
	}
	h.removeStreamPair(p.requestID)
}

// hasStreamPair returns a bool indicating if a stream pair for requestID
// exists.
func (h *httpStreamHandler) hasStreamPair(requestID string) bool {
	h.streamPairsLock.RLock()
	defer h.streamPairsLock.RUnlock()

	_, ok := h.streamPairs[requestID]
	return ok
}

// removeStreamPair removes the stream pair identified by requestID from streamPairs.
func (h *httpStreamHandler) removeStreamPair(requestID string) {
	h.streamPairsLock.Lock()
	defer h.streamPairsLock.Unlock()

	delete(h.streamPairs, requestID)
}

// requestID returns the request id for stream.
// 从stream header中获得"requestID"的值
// 如果没有requestID，则从spdy stream的id中推断从requestID（error为spdy stream的id，data为spdy stream的id减去2）
func (h *httpStreamHandler) requestID(stream httpstream.Stream) string {
	// 从stream header中获得"requestID"的值
	requestID := stream.Headers().Get(api.PortForwardRequestIDHeader)
	// 没有requestID
	if len(requestID) == 0 {
		klog.V(5).Infof("(conn=%p) stream received without %s header", h.conn, api.PortForwardRequestIDHeader)
		// If we get here, it's because the connection came from an older client
		// that isn't generating the request id header
		// (https://github.com/kubernetes/kubernetes/blob/843134885e7e0b360eb5441e85b1410a8b1a7a0c/pkg/client/unversioned/portforward/portforward.go#L258-L287)
		//
		// This is a best-effort attempt at supporting older clients.
		//
		// When there aren't concurrent new forwarded connections, each connection
		// will have a pair of streams (data, error), and the stream IDs will be
		// consecutive odd numbers, e.g. 1 and 3 for the first connection. Convert
		// the stream ID into a pseudo-request id by taking the stream type and
		// using id = stream.Identifier() when the stream type is error,
		// and id = stream.Identifier() - 2 when it's data.
		//
		// NOTE: this only works when there are not concurrent new streams from
		// multiple forwarded connections; it's a best-effort attempt at supporting
		// old clients that don't generate request ids.  If there are concurrent
		// new connections, it's possible that 1 connection gets streams whose IDs
		// are not consecutive (e.g. 5 and 9 instead of 5 and 7).
		// 从stream header中获得"streamType"的值
		streamType := stream.Headers().Get(api.StreamType)
		switch streamType {
		case api.StreamTypeError:
			// requestID为spdy stream的id
			requestID = strconv.Itoa(int(stream.Identifier()))
		case api.StreamTypeData:
			// requestID为spdy stream的id减去2
			requestID = strconv.Itoa(int(stream.Identifier()) - 2)
		}

		klog.V(5).Infof("(conn=%p) automatically assigning request ID=%q from stream type=%s, stream ID=%d", h.conn, requestID, streamType, stream.Identifier())
	}
	return requestID
}

// run is the main loop for the httpStreamHandler. It processes new
// streams, invoking portForward for each complete stream pair. The loop exits
// when the httpstream.Connection is closed.
// 等待收到新的spdy stream，直到连接关闭
// 从stream header中获得"requestID"的值
// 如果没有requestID，则从spdy stream的id中推断从requestID（error为spdy stream的id，data为spdy stream的id减去2）
// 从h.streamPairs获得requestID对应的httpStreamPair，或创建新的httpStreamPair，保存到h.streamPairs
// 如果新创建httpStreamPair，则启动一个goroutine
// 等待timeout超时，或httpStreamPair.complete消息（已经收到了error和data stream），然后从h.streamPairs移除httpStreamPair.requestID
// 根据stream的header中获得"streamType"的值
// 如果为"error"，且没有httpStreamPair.errorStream，则设置stream为httpStreamPair.errorStream，否则将错误写到httpStreamPair.errorStream
// 如果为"data"，且没有httpStreamPair.dataStream，则设置stream为httpStreamPair.dataStream，否则将错误写到httpStreamPair.errorStream
// 即有httpStreamPair.errorStream和httpStreamPair.dataStream，则关闭httpStreamPair.complete的chan
// httpStreamPair.errorStream和httpStreamPair.dataStream都收到了。则启动一个goroutine
// 从httpStreamPair.dataStream的头部获得"port"
// 查询容器是否存在
// 容器不在运行状态，直接返回错误
// 从stream中读取数据作为stdin，发送stdout数据到stream
// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
func (h *httpStreamHandler) run() {
	klog.V(5).Infof("(conn=%p) waiting for port forward streams", h.conn)
Loop:
	for {
		select {
		// 连接关闭了，则退出循环
		case <-h.conn.CloseChan():
			klog.V(5).Infof("(conn=%p) upgraded connection closed", h.conn)
			break Loop
		// 收到新的spdy stream
		case stream := <-h.streamChan:
			// 从stream header中获得"requestID"的值
			// 如果没有requestID，则从spdy stream的id中推断从requestID（error为spdy stream的id，data为spdy stream的id减去2）
			requestID := h.requestID(stream)
			// 从stream header中获得"streamType"的值
			streamType := stream.Headers().Get(api.StreamType)
			klog.V(5).Infof("(conn=%p, request=%s) received new stream of type %s", h.conn, requestID, streamType)

			// 从h.streamPairs获得requestID对应的httpStreamPair
			// 如果存在，则返回已经存在的httpStreamPair和false
			// 如果不存在，则创建新的httpStreamPair，保存到h.streamPairs，返回创建的httpStreamPair和true
			p, created := h.getStreamPair(requestID)
			// 新创建httpStreamPair，则启动一个goroutine
			// 等待timeout超时，或p.complete消息（已经收到了error和data stream，在p.add中关闭p.complete），然后从h.streamPairs移除p.requestID
			if created {
				go h.monitorStreamPair(p, time.After(h.streamCreationTimeout))
			}
			// 根据stream的header中获得"streamType"的值
			// 如果为"error"，且没有p.errorStream，则设置stream为p.errorStream，否则返回false，错误
			// 如果为"data"，且没有p.dataStream，则设置stream为p.dataStream，否则返回false，错误
			// 即有p.errorStream和p.dataStream，则关闭p.complete的chan，返回true，nil
			if complete, err := p.add(stream); err != nil {
				msg := fmt.Sprintf("error processing stream for request %s: %v", requestID, err)
				utilruntime.HandleError(errors.New(msg))
				// p.errorStream不为nil，则发送s到p.errorStream
				p.printError(msg)
			} else if complete {
				// p.errorStream和p.dataStream都收到了。则启动一个goroutine
				// 从p.dataStream的头部获得"port"
				// 查询容器是否存在
				// 容器不在运行状态，直接返回错误
				// 从stream中读取数据作为stdin，发送stdout数据到stream
				// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
				go h.portForward(p)
			}
		}
	}
}

// portForward invokes the httpStreamHandler's forwarder.PortForward
// function for the given stream pair.
func (h *httpStreamHandler) portForward(p *httpStreamPair) {
	defer p.dataStream.Close()
	defer p.errorStream.Close()

	// 从p.dataStream的头部获得"port"
	portString := p.dataStream.Headers().Get(api.PortHeader)
	port, _ := strconv.ParseInt(portString, 10, 32)

	klog.V(5).Infof("(conn=%p, request=%s) invoking forwarder.PortForward for port %s", h.conn, p.requestID, portString)
	// 查询容器是否存在
	// 容器不在运行状态，直接返回错误
	// 从stream中读取数据作为stdin，发送stdout数据到stream
	// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
	err := h.forwarder.PortForward(h.pod, h.uid, int32(port), p.dataStream)
	klog.V(5).Infof("(conn=%p, request=%s) done invoking forwarder.PortForward for port %s", h.conn, p.requestID, portString)

	if err != nil {
		msg := fmt.Errorf("error forwarding port %d to pod %s, uid %v: %v", port, h.pod, h.uid, err)
		utilruntime.HandleError(msg)
		fmt.Fprint(p.errorStream, msg.Error())
	}
}

// httpStreamPair represents the error and data streams for a port
// forwarding request.
type httpStreamPair struct {
	lock        sync.RWMutex
	requestID   string
	dataStream  httpstream.Stream
	errorStream httpstream.Stream
	complete    chan struct{}
}

// newPortForwardPair creates a new httpStreamPair.
func newPortForwardPair(requestID string) *httpStreamPair {
	return &httpStreamPair{
		requestID: requestID,
		complete:  make(chan struct{}),
	}
}

// add adds the stream to the httpStreamPair. If the pair already
// contains a stream for the new stream's type, an error is returned. add
// returns true if both the data and error streams for this pair have been
// received.
// 根据stream的header中获得"streamType"的值
// 如果为"error"，且没有p.errorStream，则设置stream为p.errorStream，否则返回false，错误
// 如果为"data"，且没有p.dataStream，则设置stream为p.dataStream，否则返回false，错误
// 即有p.errorStream和p.dataStream，则关闭p.complete的chan，返回true，nil
func (p *httpStreamPair) add(stream httpstream.Stream) (bool, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// 从stream header中获得"streamType"的值
	switch stream.Headers().Get(api.StreamType) {
	// "error"
	case api.StreamTypeError:
		// 已经有p.errorStream，则返回错误
		if p.errorStream != nil {
			return false, errors.New("error stream already assigned")
		}
		// 没有p.errorStream，则设置stream为p.errorStream
		p.errorStream = stream
	case api.StreamTypeData:
		// 已经有p.dataStream，则返回错误
		if p.dataStream != nil {
			return false, errors.New("data stream already assigned")
		}
		// 没有p.dataStream，则设置stream为p.dataStream
		p.dataStream = stream
	}

	complete := p.errorStream != nil && p.dataStream != nil
	// 即有p.errorStream和p.dataStream，则关闭p.complete的chan
	if complete {
		close(p.complete)
	}
	return complete, nil
}

// printError writes s to p.errorStream if p.errorStream has been set.
// p.errorStream不为nil，则发送s到p.errorStream
func (p *httpStreamPair) printError(s string) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if p.errorStream != nil {
		fmt.Fprint(p.errorStream, s)
	}
}
