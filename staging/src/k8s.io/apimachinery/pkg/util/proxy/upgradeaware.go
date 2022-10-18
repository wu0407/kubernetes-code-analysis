/*
Copyright 2017 The Kubernetes Authors.

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

package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/mxk/go-flowrate/flowrate"
	"k8s.io/klog"
)

// UpgradeRequestRoundTripper provides an additional method to decorate a request
// with any authentication or other protocol level information prior to performing
// an upgrade on the server. Any response will be handled by the intercepting
// proxy.
type UpgradeRequestRoundTripper interface {
	http.RoundTripper
	// WrapRequest takes a valid HTTP request and returns a suitably altered version
	// of request with any HTTP level values required to complete the request half of
	// an upgrade on the server. It does not get a chance to see the response and
	// should bypass any request side logic that expects to see the response.
	WrapRequest(*http.Request) (*http.Request, error)
}

// UpgradeAwareHandler is a handler for proxy requests that may require an upgrade
type UpgradeAwareHandler struct {
	// UpgradeRequired will reject non-upgrade connections if true.
	UpgradeRequired bool
	// Location is the location of the upstream proxy. It is used as the location to Dial on the upstream server
	// for upgrade requests unless UseRequestLocationOnUpgrade is true.
	Location *url.URL
	// Transport provides an optional round tripper to use to proxy. If nil, the default proxy transport is used
	Transport http.RoundTripper
	// UpgradeTransport, if specified, will be used as the backend transport when upgrade requests are provided.
	// This allows clients to disable HTTP/2.
	UpgradeTransport UpgradeRequestRoundTripper
	// WrapTransport indicates whether the provided Transport should be wrapped with default proxy transport behavior (URL rewriting, X-Forwarded-* header setting)
	WrapTransport bool
	// InterceptRedirects determines whether the proxy should sniff backend responses for redirects,
	// following them as necessary.
	InterceptRedirects bool
	// RequireSameHostRedirects only allows redirects to the same host. It is only used if InterceptRedirects=true.
	RequireSameHostRedirects bool
	// UseRequestLocation will use the incoming request URL when talking to the backend server.
	UseRequestLocation bool
	// FlushInterval controls how often the standard HTTP proxy will flush content from the upstream.
	FlushInterval time.Duration
	// MaxBytesPerSec controls the maximum rate for an upstream connection. No rate is imposed if the value is zero.
	MaxBytesPerSec int64
	// Responder is passed errors that occur while setting up proxying.
	Responder ErrorResponder
}

const defaultFlushInterval = 200 * time.Millisecond

// ErrorResponder abstracts error reporting to the proxy handler to remove the need to hardcode a particular
// error format.
type ErrorResponder interface {
	Error(w http.ResponseWriter, req *http.Request, err error)
}

// SimpleErrorResponder is the legacy implementation of ErrorResponder for callers that only
// service a single request/response per proxy.
type SimpleErrorResponder interface {
	Error(err error)
}

func NewErrorResponder(r SimpleErrorResponder) ErrorResponder {
	return simpleResponder{r}
}

type simpleResponder struct {
	responder SimpleErrorResponder
}

func (r simpleResponder) Error(w http.ResponseWriter, req *http.Request, err error) {
	r.responder.Error(err)
}

// upgradeRequestRoundTripper implements proxy.UpgradeRequestRoundTripper.
type upgradeRequestRoundTripper struct {
	http.RoundTripper
	upgrader http.RoundTripper
}

var (
	_ UpgradeRequestRoundTripper  = &upgradeRequestRoundTripper{}
	_ utilnet.RoundTripperWrapper = &upgradeRequestRoundTripper{}
)

// WrappedRoundTripper returns the round tripper that a caller would use.
func (rt *upgradeRequestRoundTripper) WrappedRoundTripper() http.RoundTripper {
	return rt.RoundTripper
}

// WriteToRequest calls the nested upgrader and then copies the returned request
// fields onto the passed request.
func (rt *upgradeRequestRoundTripper) WrapRequest(req *http.Request) (*http.Request, error) {
	resp, err := rt.upgrader.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return resp.Request, nil
}

// onewayRoundTripper captures the provided request - which is assumed to have
// been modified by other round trippers - and then returns a fake response.
type onewayRoundTripper struct{}

// RoundTrip returns a simple 200 OK response that captures the provided request.
func (onewayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(&bytes.Buffer{}),
		Request:    req,
	}, nil
}

// MirrorRequest is a round tripper that can be called to get back the calling request as
// the core round tripper in a chain.
var MirrorRequest http.RoundTripper = onewayRoundTripper{}

// NewUpgradeRequestRoundTripper takes two round trippers - one for the underlying TCP connection, and
// one that is able to write headers to an HTTP request. The request rt is used to set the request headers
// and that is written to the underlying connection rt.
func NewUpgradeRequestRoundTripper(connection, request http.RoundTripper) UpgradeRequestRoundTripper {
	return &upgradeRequestRoundTripper{
		RoundTripper: connection,
		upgrader:     request,
	}
}

// normalizeLocation returns the result of parsing the full URL, with scheme set to http if missing
// 如果url里没有协议类型，则设置为http协议
func normalizeLocation(location *url.URL) *url.URL {
	normalized, _ := url.Parse(location.String())
	if len(normalized.Scheme) == 0 {
		normalized.Scheme = "http"
	}
	return normalized
}

// NewUpgradeAwareHandler creates a new proxy handler with a default flush interval. Responder is required for returning
// errors to the caller.
func NewUpgradeAwareHandler(location *url.URL, transport http.RoundTripper, wrapTransport, upgradeRequired bool, responder ErrorResponder) *UpgradeAwareHandler {
	return &UpgradeAwareHandler{
		// 如果url里没有协议类型，则设置为http协议
		Location:        normalizeLocation(location),
		Transport:       transport,
		WrapTransport:   wrapTransport,
		UpgradeRequired: upgradeRequired,
		FlushInterval:   defaultFlushInterval,
		Responder:       responder,
	}
}

// ServeHTTP handles the proxy request
func (h *UpgradeAwareHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// 代理Upgrade请求（websocket），返回是否处理了这个请求
	if h.tryUpgrade(w, req) {
		return
	}
	// 需要进行Upgrade请求（websocket）且请求不是Upgrade请求，则返回400
	if h.UpgradeRequired {
		h.Responder.Error(w, req, errors.NewBadRequest("Upgrade request required"))
		return
	}

	loc := *h.Location
	loc.RawQuery = req.URL.RawQuery

	// If original request URL ended in '/', append a '/' at the end of the
	// of the proxy URL
	// location（代理）请求里的路径后缀里没有"/"，且（用户）req里的路径后缀有"/"，则local请求里的路径加上"/"
	if !strings.HasSuffix(loc.Path, "/") && strings.HasSuffix(req.URL.Path, "/") {
		loc.Path += "/"
	}

	// From pkg/genericapiserver/endpoints/handlers/proxy.go#ServeHTTP:
	// Redirect requests with an empty path to a location that ends with a '/'
	// This is essentially a hack for http://issue.k8s.io/4958.
	// Note: Keep this code after tryUpgrade to not break that flow.
	// location路径为""，则响应header里的"Location"值为"{req.URL.Path}+"/"+{queryPart}"（让客户端重新发起请求路径为"/"）
	// 返回http code 301
	if len(loc.Path) == 0 {
		var queryPart string
		if len(req.URL.RawQuery) > 0 {
			queryPart = "?" + req.URL.RawQuery
		}
		w.Header().Set("Location", req.URL.Path+"/"+queryPart)
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}

	// 没有h.Transport或h.WrapTransport（需要包装transport）
	if h.Transport == nil || h.WrapTransport {
		// corsRemovingTransport为包装了h.Transport，同时具有在响应时移除cors的header头
		h.Transport = h.defaultProxyTransport(req.URL, h.Transport)
	}

	// WithContext creates a shallow clone of the request with the same context.
	// 复制用户的request，生成对后端进行请求的request
	newReq := req.WithContext(req.Context())
	newReq.Header = utilnet.CloneHeader(req.Header)
	// h.UseRequestLocation为false（不使用用请求路径访问后端），则使用location路径访问后端
	if !h.UseRequestLocation {
		newReq.URL = &loc
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: h.Location.Scheme, Host: h.Location.Host})
	proxy.Transport = h.Transport
	proxy.FlushInterval = h.FlushInterval
	proxy.ErrorLog = log.New(noSuppressPanicError{}, "", log.LstdFlags)
	proxy.ServeHTTP(w, newReq)
}

type noSuppressPanicError struct{}

func (noSuppressPanicError) Write(p []byte) (n int, err error) {
	// skip "suppressing panic for copyResponse error in test; copy error" error message
	// that ends up in CI tests on each kube-apiserver termination as noise and
	// everybody thinks this is fatal.
	if strings.Contains(string(p), "suppressing panic") {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// tryUpgrade returns true if the request was handled.
// 代理Upgrade请求（websocket），返回是否处理了这个请求
func (h *UpgradeAwareHandler) tryUpgrade(w http.ResponseWriter, req *http.Request) bool {
	// http request中的header里的"Connection"值，没有包含"Upgrade"，则返回false
	if !httpstream.IsUpgradeRequest(req) {
		klog.V(6).Infof("Request was not an upgrade")
		return false
	}

	var (
		backendConn net.Conn
		rawResponse []byte
		err         error
	)

	location := *h.Location
	// 如果使用请求的url跟后端通信
	if h.UseRequestLocation {
		location = *req.URL
		location.Scheme = h.Location.Scheme
		location.Host = h.Location.Host
	}

	clone := utilnet.CloneRequest(req)
	// Only append X-Forwarded-For in the upgrade path, since httputil.NewSingleHostReverseProxy
	// handles this in the non-upgrade path.
	// http header中已经有"X-Forwarded-For"，则用逗号分隔之前的值，并添加现在客户端ip的值
	utilnet.AppendForwardedForHeader(clone)
	// 启用解析后端返回的重定向
	if h.InterceptRedirects {
		klog.V(6).Infof("Connecting to backend proxy (intercepting redirects) %s\n  Headers: %v", &location, clone.Header)
		// h.DialForUpgrade
		//   对于Upgrade请求进行
		//   h.UpgradeTransport为nil，则使用h.Transport
		//   不为nil，则包装request（比如先进行认证）
		//   进行tcp或tls连接
		//   写入http请求
		//
		// 进行最多10次重定向请求，返回net.Conn和原始响应byte
		backendConn, rawResponse, err = utilnet.ConnectWithRedirects(req.Method, &location, clone.Header, req.Body, utilnet.DialerFunc(h.DialForUpgrade), h.RequireSameHostRedirects)
	} else {
		klog.V(6).Infof("Connecting to backend proxy (direct dial) %s\n  Headers: %v", &location, clone.Header)
		clone.URL = &location
		backendConn, err = h.DialForUpgrade(clone)
	}
	if err != nil {
		klog.V(6).Infof("Proxy connection error: %v", err)
		h.Responder.Error(w, req, err)
		return true
	}
	defer backendConn.Close()

	// determine the http response code from the backend by reading from rawResponse+backendConn
	// 从rawResponse和backendConn中读取http response和原始的前256个byte
	backendHTTPResponse, headerBytes, err := getResponse(io.MultiReader(bytes.NewReader(rawResponse), backendConn))
	if err != nil {
		klog.V(6).Infof("Proxy connection error: %v", err)
		h.Responder.Error(w, req, err)
		return true
	}
	if len(headerBytes) > len(rawResponse) {
		// we read beyond the bytes stored in rawResponse, update rawResponse to the full set of bytes read from the backend
		rawResponse = headerBytes
	}

	// If the backend did not upgrade the request, return an error to the client. If the response was
	// an error, the error is forwarded directly after the connection is hijacked. Otherwise, just
	// return a generic error here.
	// 后端的http响应码不为101且小于400
	if backendHTTPResponse.StatusCode != http.StatusSwitchingProtocols && backendHTTPResponse.StatusCode < 400 {
		err := fmt.Errorf("invalid upgrade response: status code %d", backendHTTPResponse.StatusCode)
		klog.Errorf("Proxy upgrade error: %v", err)
		h.Responder.Error(w, req, err)
		return true
	}

	// Once the connection is hijacked, the ErrorResponder will no longer work, so
	// hijacking should be the last step in the upgrade.
	requestHijacker, ok := w.(http.Hijacker)
	if !ok {
		klog.V(6).Infof("Unable to hijack response writer: %T", w)
		h.Responder.Error(w, req, fmt.Errorf("request connection cannot be hijacked: %T", w))
		return true
	}
	requestHijackedConn, _, err := requestHijacker.Hijack()
	if err != nil {
		klog.V(6).Infof("Unable to hijack response: %v", err)
		h.Responder.Error(w, req, fmt.Errorf("error hijacking connection: %v", err))
		return true
	}
	defer requestHijackedConn.Close()

	// 后端的http响应码不为101且大于等于400，则发送response到客户端
	if backendHTTPResponse.StatusCode != http.StatusSwitchingProtocols {
		// If the backend did not upgrade the request, echo the response from the backend to the client and return, closing the connection.
		klog.V(6).Infof("Proxy upgrade error, status code %d", backendHTTPResponse.StatusCode)
		// set read/write deadlines
		deadline := time.Now().Add(10 * time.Second)
		backendConn.SetReadDeadline(deadline)
		requestHijackedConn.SetWriteDeadline(deadline)
		// write the response to the client
		// 发送response到客户端
		err := backendHTTPResponse.Write(requestHijackedConn)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			klog.Errorf("Error proxying data from backend to client: %v", err)
		}
		// Indicate we handled the request
		return true
	}

	// 后端的http响应码为101

	// Forward raw response bytes back to client.
	// rawResponse有数据，发送response到客户端
	if len(rawResponse) > 0 {
		klog.V(6).Infof("Writing %d bytes to hijacked connection", len(rawResponse))
		if _, err = requestHijackedConn.Write(rawResponse); err != nil {
			utilruntime.HandleError(fmt.Errorf("Error proxying response from backend to client: %v", err))
		}
	}

	// Proxy the connection. This is bidirectional, so we need a goroutine
	// to copy in each direction. Once one side of the connection exits, we
	// exit the function which performs cleanup and in the process closes
	// the other half of the connection in the defer.
	writerComplete := make(chan struct{})
	readerComplete := make(chan struct{})

	// 将用户请求转发到后端
	go func() {
		var writer io.WriteCloser
		// 设置了最大每秒传输
		if h.MaxBytesPerSec > 0 {
			writer = flowrate.NewWriter(backendConn, h.MaxBytesPerSec)
		} else {
			writer = backendConn
		}
		// 从requestHijackedConn复制到writer
		_, err := io.Copy(writer, requestHijackedConn)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			klog.Errorf("Error proxying data from client to backend: %v", err)
		}
		close(writerComplete)
	}()

	// 将后端响应转发给用户
	go func() {
		var reader io.ReadCloser
		// 设置了最大每秒传输
		if h.MaxBytesPerSec > 0 {
			reader = flowrate.NewReader(backendConn, h.MaxBytesPerSec)
		} else {
			reader = backendConn
		}
		_, err := io.Copy(requestHijackedConn, reader)
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			klog.Errorf("Error proxying data from backend to client: %v", err)
		}
		close(readerComplete)
	}()

	// Wait for one half the connection to exit. Once it does the defer will
	// clean up the other half of the connection.
	select {
	case <-writerComplete:
	case <-readerComplete:
	}
	klog.V(6).Infof("Disconnecting from backend proxy %s\n  Headers: %v", &location, clone.Header)

	return true
}

// 对于Upgrade请求进行
// h.UpgradeTransport为nil，则使用h.Transport
// 不为nil，则包装request（比如先进行认证）
// 进行tcp或tls连接
// 写入http请求
func (h *UpgradeAwareHandler) DialForUpgrade(req *http.Request) (net.Conn, error) {
	// 不进行UpgradeTransport，则使用h.Transport
	// 进行tcp或tls连接
	// 写入http请求
	if h.UpgradeTransport == nil {
		return dial(req, h.Transport)
	}
	// 包装request（比如先进行认证）
	updatedReq, err := h.UpgradeTransport.WrapRequest(req)
	if err != nil {
		return nil, err
	}
	// 进行tcp或tls连接
	// 写入http请求
	return dial(updatedReq, h.UpgradeTransport)
}

// getResponseCode reads a http response from the given reader, returns the response,
// the bytes read from the reader, and any error encountered
// 从r中读取http response，和原始的前256个byte
func getResponse(r io.Reader) (*http.Response, []byte, error) {
	rawResponse := bytes.NewBuffer(make([]byte, 0, 256))
	// Save the bytes read while reading the response headers into the rawResponse buffer
	resp, err := http.ReadResponse(bufio.NewReader(io.TeeReader(r, rawResponse)), nil)
	if err != nil {
		return nil, nil, err
	}
	// return the http response and the raw bytes consumed from the reader in the process
	return resp, rawResponse.Bytes(), nil
}

// dial dials the backend at req.URL and writes req to it.
// 进行tcp或tls连接
// 写入http请求
func dial(req *http.Request, transport http.RoundTripper) (net.Conn, error) {
	// 进行tcp或tls连接
	conn, err := dialURL(req.Context(), req.URL, transport)
	if err != nil {
		return nil, fmt.Errorf("error dialing backend: %v", err)
	}

	// 写入http请求
	if err = req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("error sending request: %v", err)
	}

	return conn, err
}

func (h *UpgradeAwareHandler) defaultProxyTransport(url *url.URL, internalTransport http.RoundTripper) http.RoundTripper {
	scheme := url.Scheme
	host := url.Host
	suffix := h.Location.Path
	// （用户请求）url.Path后缀有"/"，（代理请求）h.Location.Path不为"/"，则suffix添加后缀"/"
	if strings.HasSuffix(url.Path, "/") && !strings.HasSuffix(suffix, "/") {
		suffix += "/"
	}
	// 用户请求路径中移除代理请求路径
	pathPrepend := strings.TrimSuffix(url.Path, suffix)
	rewritingTransport := &Transport{
		Scheme:       scheme,
		Host:         host,
		PathPrepend:  pathPrepend,
		RoundTripper: internalTransport,
	}
	return &corsRemovingTransport{
		RoundTripper: rewritingTransport,
	}
}

// corsRemovingTransport is a wrapper for an internal transport. It removes CORS headers
// from the internal response.
// Implements pkg/util/net.RoundTripperWrapper
type corsRemovingTransport struct {
	http.RoundTripper
}

var _ = utilnet.RoundTripperWrapper(&corsRemovingTransport{})

func (rt *corsRemovingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	removeCORSHeaders(resp)
	return resp, nil
}

func (rt *corsRemovingTransport) WrappedRoundTripper() http.RoundTripper {
	return rt.RoundTripper
}

// removeCORSHeaders strip CORS headers sent from the backend
// This should be called on all responses before returning
func removeCORSHeaders(resp *http.Response) {
	resp.Header.Del("Access-Control-Allow-Credentials")
	resp.Header.Del("Access-Control-Allow-Headers")
	resp.Header.Del("Access-Control-Allow-Methods")
	resp.Header.Del("Access-Control-Allow-Origin")
}
