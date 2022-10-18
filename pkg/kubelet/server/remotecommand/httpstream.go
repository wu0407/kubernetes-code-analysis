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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/util/wsstream"
	"k8s.io/client-go/tools/remotecommand"
	api "k8s.io/kubernetes/pkg/apis/core"

	"k8s.io/klog"
)

// Options contains details about which streams are required for
// remote command execution.
type Options struct {
	Stdin  bool
	Stdout bool
	Stderr bool
	TTY    bool
}

// NewOptions creates a new Options from the Request.
// 根据http.request解析出是否启用tty、stdin、stdout、stderr
func NewOptions(req *http.Request) (*Options, error) {
	tty := req.FormValue(api.ExecTTYParam) == "1"
	stdin := req.FormValue(api.ExecStdinParam) == "1"
	stdout := req.FormValue(api.ExecStdoutParam) == "1"
	stderr := req.FormValue(api.ExecStderrParam) == "1"
	// 不支持同时启用tty和stderr，如果同时启用，则设置stderr不启用
	if tty && stderr {
		// TODO: make this an error before we reach this method
		klog.V(4).Infof("Access to exec with tty and stderr is not supported, bypassing stderr")
		stderr = false
	}

	// stdin、stdout、stderr必须启用一个
	if !stdin && !stdout && !stderr {
		return nil, fmt.Errorf("you must specify at least 1 of stdin, stdout, stderr")
	}

	return &Options{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		TTY:    tty,
	}, nil
}

// context contains the connection and streams used when
// forwarding an attach or execute session into a container.
type context struct {
	conn         io.Closer
	stdinStream  io.ReadCloser
	stdoutStream io.WriteCloser
	stderrStream io.WriteCloser
	writeStatus  func(status *apierrors.StatusError) error
	resizeStream io.ReadCloser
	resizeChan   chan remotecommand.TerminalSize
	tty          bool
}

// streamAndReply holds both a Stream and a channel that is closed when the stream's reply frame is
// enqueued. Consumers can wait for replySent to be closed prior to proceeding, to ensure that the
// replyFrame is enqueued before the connection's goaway frame is sent (e.g. if a stream was
// received and right after, the connection gets closed).
type streamAndReply struct {
	httpstream.Stream
	replySent <-chan struct{}
}

// waitStreamReply waits until either replySent or stop is closed. If replySent is closed, it sends
// an empty struct to the notify channel.
// 从replySent收到消息，通知给notify
// 如果收到stop消息，则退出
func waitStreamReply(replySent <-chan struct{}, notify chan<- struct{}, stop <-chan struct{}) {
	select {
	case <-replySent:
		notify <- struct{}{}
	case <-stop:
	}
}

// 创建stream server，返回包含各个fd stream的context
func createStreams(req *http.Request, w http.ResponseWriter, opts *Options, supportedStreamProtocols []string, idleTimeout, streamCreationTimeout time.Duration) (*context, bool) {
	var ctx *context
	var ok bool
	// header中"Upgrade"的值为"websocket"，且header中"Connection"匹配"(^|.*,\\s*)upgrade($|\\s*,)"
	if wsstream.IsWebSocketRequest(req) {
		// 创建websocket connection和streams，用来执行exec和attach
		ctx, ok = createWebSocketStreams(req, w, opts, idleTimeout)
	} else {
		// supportedStreamProtocols默认为["v4.channel.k8s.io", "v3.channel.k8s.io", "v2.channel.k8s.io", "channel.k8s.io"]
		// 创建"SPDY/3.1" spdystream.Connection，用来执行exec和attach
		ctx, ok = createHTTPStreamStreams(req, w, opts, supportedStreamProtocols, idleTimeout, streamCreationTimeout)
	}
	if !ok {
		return nil, false
	}

	if ctx.resizeStream != nil {
		ctx.resizeChan = make(chan remotecommand.TerminalSize)
		// 启动一个goroutine
		// 一直从ctx.resizeStream中json解析出remotecommand.TerminalSize（宽度和高度）发送到ctx.resizeChan
		// json解析遇到错误直接退出
		go handleResizeEvents(ctx.resizeStream, ctx.resizeChan)
	}

	return ctx, true
}

func createHTTPStreamStreams(req *http.Request, w http.ResponseWriter, opts *Options, supportedStreamProtocols []string, idleTimeout, streamCreationTimeout time.Duration) (*context, bool) {
	// 从请求的头部"X-Stream-Protocol-Version"值，找到第一个在serverProtocols中的protocol
	// 如果找不到，则响应header中添加"X-Accepted-Stream-Protocol-Versions" header头部，值为服务端支持的protocol，http code为403
	// 否则响应header中添加key为"X-Stream-Protocol-Version"，值为第一个支持的版本
	// 返回第一个支持的版本
	protocol, err := httpstream.Handshake(req, w, supportedStreamProtocols)
	if err != nil {
		// 客户端没有提供请求的头部"X-Stream-Protocol-Version"，或没有值，则返回http 400
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}

	streamCh := make(chan streamAndReply)

	upgrader := spdy.NewResponseUpgrader()
	// 校验请求header中"Connection"的值是否为"Upgrade"，header中"Upgrade"的值是否为"SPDY/3.1"
	// 如果请求合法，则响应的http header添加key为"Connection"，value为"Upgrade"和key为"Upgrade"，value为"SPDY/3.1"和http code为101
	// 创建一个goroutine来服务stream frames
	// 返回包装了spdystream.Connection的connection
	conn := upgrader.UpgradeResponse(w, req, func(stream httpstream.Stream, replySent <-chan struct{}) error {
		// 新的stream，就发送streamAndReply到streamCh
		streamCh <- streamAndReply{Stream: stream, replySent: replySent}
		return nil
	})
	// from this point on, we can no longer call methods on response
	if conn == nil {
		// The upgrader is responsible for notifying the client of any errors that
		// occurred during upgrading. All we can do is return here at this point
		// if we weren't successful in upgrading.
		return nil, false
	}

	// 给底层的spdystream.Connection设置超时时间
	conn.SetIdleTimeout(idleTimeout)

	var handler protocolHandler
	switch protocol {
	case remotecommandconsts.StreamProtocolV4Name:
		handler = &v4ProtocolHandler{}
	case remotecommandconsts.StreamProtocolV3Name:
		handler = &v3ProtocolHandler{}
	case remotecommandconsts.StreamProtocolV2Name:
		handler = &v2ProtocolHandler{}
	case "":
		klog.V(4).Infof("Client did not request protocol negotiation. Falling back to %q", remotecommandconsts.StreamProtocolV1Name)
		fallthrough
	case remotecommandconsts.StreamProtocolV1Name:
		handler = &v1ProtocolHandler{}
	}

	// count the streams client asked for, starting with 1
	expectedStreams := 1
	if opts.Stdin {
		expectedStreams++
	}
	if opts.Stdout {
		expectedStreams++
	}
	if opts.Stderr {
		expectedStreams++
	}
	// 除了v1ProtocolHandler不支持TerminalResizing，其他handler都支持TerminalResizing
	if opts.TTY && handler.supportsTerminalResizing() {
		expectedStreams++
	}

	expired := time.NewTimer(streamCreationTimeout)
	defer expired.Stop()

	// 如果handler是v4ProtocolHandler
	// 等待所有期望的stream都已经接受和响应，返回所有包含所有stream的ctx
	ctx, err := handler.waitForStreams(streamCh, expectedStreams, expired.C)
	if err != nil {
		runtime.HandleError(err)
		return nil, false
	}

	ctx.conn = conn
	ctx.tty = opts.TTY

	return ctx, true
}

type protocolHandler interface {
	// waitForStreams waits for the expected streams or a timeout, returning a
	// remoteCommandContext if all the streams were received, or an error if not.
	waitForStreams(streams <-chan streamAndReply, expectedStreams int, expired <-chan time.Time) (*context, error)
	// supportsTerminalResizing returns true if the protocol handler supports terminal resizing
	supportsTerminalResizing() bool
}

// v4ProtocolHandler implements the V4 protocol version for streaming command execution. It only differs
// in from v3 in the error stream format using an json-marshaled metav1.Status which carries
// the process' exit code.
type v4ProtocolHandler struct{}

func (*v4ProtocolHandler) waitForStreams(streams <-chan streamAndReply, expectedStreams int, expired <-chan time.Time) (*context, error) {
	ctx := &context{}
	receivedStreams := 0
	replyChan := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
WaitForStreams:
	for {
		select {
		// stream被接受并进行空header的响应
		case stream := <-streams:
			// spdystream.Stream的头部里"streamType"的值
			streamType := stream.Headers().Get(api.StreamType)
			switch streamType {
			// "error"
			case api.StreamTypeError:
				// 发送status.ErrStatus到spdystream.Stream
				ctx.writeStatus = v4WriteStatusFunc(stream) // write json errors
				// 启动一个goroutine
				// 从replySent收到消息，通知给notify
				// 如果收到stop消息，则退出
				go waitStreamReply(stream.replySent, replyChan, stop)
			// "stdin"
			case api.StreamTypeStdin:
				ctx.stdinStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			// "stdout"
			case api.StreamTypeStdout:
				ctx.stdoutStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			// "stderr"
			case api.StreamTypeStderr:
				ctx.stderrStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			// "resize"
			case api.StreamTypeResize:
				ctx.resizeStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			default:
				runtime.HandleError(fmt.Errorf("unexpected stream type: %q", streamType))
			}
		// 如果已经进行响应
		case <-replyChan:
			receivedStreams++
			// 接收到的stream数量等于期望的数量，则退出循环
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		// 如果stream创建超时了
		case <-expired:
			// TODO find a way to return the error to the user. Maybe use a separate
			// stream to report errors?
			return nil, errors.New("timed out waiting for client to create streams")
		}
	}

	return ctx, nil
}

// supportsTerminalResizing returns true because v4ProtocolHandler supports it
func (*v4ProtocolHandler) supportsTerminalResizing() bool { return true }

// v3ProtocolHandler implements the V3 protocol version for streaming command execution.
type v3ProtocolHandler struct{}

func (*v3ProtocolHandler) waitForStreams(streams <-chan streamAndReply, expectedStreams int, expired <-chan time.Time) (*context, error) {
	ctx := &context{}
	receivedStreams := 0
	replyChan := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
WaitForStreams:
	for {
		select {
		case stream := <-streams:
			streamType := stream.Headers().Get(api.StreamType)
			switch streamType {
			case api.StreamTypeError:
				ctx.writeStatus = v1WriteStatusFunc(stream)
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdin:
				ctx.stdinStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdout:
				ctx.stdoutStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStderr:
				ctx.stderrStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeResize:
				ctx.resizeStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			default:
				runtime.HandleError(fmt.Errorf("unexpected stream type: %q", streamType))
			}
		case <-replyChan:
			receivedStreams++
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		case <-expired:
			// TODO find a way to return the error to the user. Maybe use a separate
			// stream to report errors?
			return nil, errors.New("timed out waiting for client to create streams")
		}
	}

	return ctx, nil
}

// supportsTerminalResizing returns true because v3ProtocolHandler supports it
func (*v3ProtocolHandler) supportsTerminalResizing() bool { return true }

// v2ProtocolHandler implements the V2 protocol version for streaming command execution.
type v2ProtocolHandler struct{}

func (*v2ProtocolHandler) waitForStreams(streams <-chan streamAndReply, expectedStreams int, expired <-chan time.Time) (*context, error) {
	ctx := &context{}
	receivedStreams := 0
	replyChan := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
WaitForStreams:
	for {
		select {
		case stream := <-streams:
			streamType := stream.Headers().Get(api.StreamType)
			switch streamType {
			case api.StreamTypeError:
				ctx.writeStatus = v1WriteStatusFunc(stream)
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdin:
				ctx.stdinStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdout:
				ctx.stdoutStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStderr:
				ctx.stderrStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			default:
				runtime.HandleError(fmt.Errorf("unexpected stream type: %q", streamType))
			}
		case <-replyChan:
			receivedStreams++
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		case <-expired:
			// TODO find a way to return the error to the user. Maybe use a separate
			// stream to report errors?
			return nil, errors.New("timed out waiting for client to create streams")
		}
	}

	return ctx, nil
}

// supportsTerminalResizing returns false because v2ProtocolHandler doesn't support it.
func (*v2ProtocolHandler) supportsTerminalResizing() bool { return false }

// v1ProtocolHandler implements the V1 protocol version for streaming command execution.
type v1ProtocolHandler struct{}

func (*v1ProtocolHandler) waitForStreams(streams <-chan streamAndReply, expectedStreams int, expired <-chan time.Time) (*context, error) {
	ctx := &context{}
	receivedStreams := 0
	replyChan := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
WaitForStreams:
	for {
		select {
		case stream := <-streams:
			streamType := stream.Headers().Get(api.StreamType)
			switch streamType {
			case api.StreamTypeError:
				ctx.writeStatus = v1WriteStatusFunc(stream)

				// This defer statement shouldn't be here, but due to previous refactoring, it ended up in
				// here. This is what 1.0.x kubelets do, so we're retaining that behavior. This is fixed in
				// the v2ProtocolHandler.
				defer stream.Reset()

				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdin:
				ctx.stdinStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStdout:
				ctx.stdoutStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			case api.StreamTypeStderr:
				ctx.stderrStream = stream
				go waitStreamReply(stream.replySent, replyChan, stop)
			default:
				runtime.HandleError(fmt.Errorf("unexpected stream type: %q", streamType))
			}
		case <-replyChan:
			receivedStreams++
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		case <-expired:
			// TODO find a way to return the error to the user. Maybe use a separate
			// stream to report errors?
			return nil, errors.New("timed out waiting for client to create streams")
		}
	}

	if ctx.stdinStream != nil {
		ctx.stdinStream.Close()
	}

	return ctx, nil
}

// supportsTerminalResizing returns false because v1ProtocolHandler doesn't support it.
func (*v1ProtocolHandler) supportsTerminalResizing() bool { return false }

// 一直从stream中json解析出remotecommand.TerminalSize（宽度和高度）发送到channel
// json解析遇到错误直接退出
func handleResizeEvents(stream io.Reader, channel chan<- remotecommand.TerminalSize) {
	defer runtime.HandleCrash()

	decoder := json.NewDecoder(stream)
	for {
		size := remotecommand.TerminalSize{}
		if err := decoder.Decode(&size); err != nil {
			break
		}
		channel <- size
	}
}

// 只发送status.ErrStatus.Message
func v1WriteStatusFunc(stream io.Writer) func(status *apierrors.StatusError) error {
	return func(status *apierrors.StatusError) error {
		if status.Status().Status == metav1.StatusSuccess {
			return nil // send error messages
		}
		_, err := stream.Write([]byte(status.Error()))
		return err
	}
}

// v4WriteStatusFunc returns a WriteStatusFunc that marshals a given api Status
// as json in the error channel.
// 发送status.ErrStatus
func v4WriteStatusFunc(stream io.Writer) func(status *apierrors.StatusError) error {
	return func(status *apierrors.StatusError) error {
		bs, err := json.Marshal(status.Status())
		if err != nil {
			return err
		}
		_, err = stream.Write(bs)
		return err
	}
}
