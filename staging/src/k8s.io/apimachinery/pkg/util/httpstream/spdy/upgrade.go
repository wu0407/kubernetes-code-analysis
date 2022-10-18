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

package spdy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/runtime"
)

const HeaderSpdy31 = "SPDY/3.1"

// responseUpgrader knows how to upgrade HTTP responses. It
// implements the httpstream.ResponseUpgrader interface.
type responseUpgrader struct {
}

// connWrapper is used to wrap a hijacked connection and its bufio.Reader. All
// calls will be handled directly by the underlying net.Conn with the exception
// of Read and Close calls, which will consider data in the bufio.Reader. This
// ensures that data already inside the used bufio.Reader instance is also
// read.
type connWrapper struct {
	net.Conn
	closed    int32
	bufReader *bufio.Reader
}

func (w *connWrapper) Read(b []byte) (n int, err error) {
	if atomic.LoadInt32(&w.closed) == 1 {
		return 0, io.EOF
	}
	return w.bufReader.Read(b)
}

func (w *connWrapper) Close() error {
	err := w.Conn.Close()
	atomic.StoreInt32(&w.closed, 1)
	return err
}

// NewResponseUpgrader returns a new httpstream.ResponseUpgrader that is
// capable of upgrading HTTP responses using SPDY/3.1 via the
// spdystream package.
func NewResponseUpgrader() httpstream.ResponseUpgrader {
	return responseUpgrader{}
}

// UpgradeResponse upgrades an HTTP response to one that supports multiplexed
// streams. newStreamHandler will be called synchronously whenever the
// other end of the upgraded connection creates a new stream.
// 校验请求header中"Connection"的值是否为"Upgrade"，header中"Upgrade"的值是否为"SPDY/3.1"
// 如果请求合法，则响应的http header添加key为"Connection"，value为"Upgrade"和key为"Upgrade"，value为"SPDY/3.1"和http code为101
// 创建一个goroutine来服务stream frames
// 返回包装了spdystream.Connection的connection
func (u responseUpgrader) UpgradeResponse(w http.ResponseWriter, req *http.Request, newStreamHandler httpstream.NewStreamHandler) httpstream.Connection {
	// header中"Connection"的值
	connectionHeader := strings.ToLower(req.Header.Get(httpstream.HeaderConnection))
	// header中"Upgrade"的值
	upgradeHeader := strings.ToLower(req.Header.Get(httpstream.HeaderUpgrade))
	// header中"Connection"的值不为"Upgrade"，或header中"Upgrade"的值不为"SPDY/3.1"，则返回http 400
	if !strings.Contains(connectionHeader, strings.ToLower(httpstream.HeaderUpgrade)) || !strings.Contains(upgradeHeader, strings.ToLower(HeaderSpdy31)) {
		errorMsg := fmt.Sprintf("unable to upgrade: missing upgrade headers in request: %#v", req.Header)
		http.Error(w, errorMsg, http.StatusBadRequest)
		return nil
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		errorMsg := fmt.Sprintf("unable to upgrade: unable to hijack response")
		http.Error(w, errorMsg, http.StatusInternalServerError)
		return nil
	}

	// 响应的http header添加key为"Connection"，value为"Upgrade"
	w.Header().Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	// 响应的http header添加key为"Upgrade"，value为"SPDY/3.1"
	w.Header().Add(httpstream.HeaderUpgrade, HeaderSpdy31)
	// 响应http code为101
	w.WriteHeader(http.StatusSwitchingProtocols)

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		runtime.HandleError(fmt.Errorf("unable to upgrade: error hijacking response: %v", err))
		return nil
	}

	connWithBuf := &connWrapper{Conn: conn, bufReader: bufrw.Reader}
	// 创建一个goroutine来服务stream frames
	// 返回包装了spdystream.Connection的connection
	spdyConn, err := NewServerConnection(connWithBuf, newStreamHandler)
	if err != nil {
		runtime.HandleError(fmt.Errorf("unable to upgrade: error creating SPDY server connection: %v", err))
		return nil
	}

	return spdyConn
}
