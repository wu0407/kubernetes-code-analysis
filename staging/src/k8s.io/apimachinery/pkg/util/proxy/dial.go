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

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"k8s.io/klog"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/third_party/forked/golang/netutil"
)

// dialURL will dial the specified URL using the underlying dialer held by the passed
// RoundTripper. The primary use of this method is to support proxying upgradable connections.
// For this reason this method will prefer to negotiate http/1.1 if the URL scheme is https.
// If you wish to ensure ALPN negotiates http2 then set NextProto=[]string{"http2"} in the
// TLSConfig of the http.Transport
func dialURL(ctx context.Context, url *url.URL, transport http.RoundTripper) (net.Conn, error) {
	// 返回url.Host转成"ip:port"这种格式
	dialAddr := netutil.CanonicalAddr(url)

	// transport为nil，则返回nil
	// transport为*http.Transport，则优先使用transport.DialContext，否则使用transport.Dial
	// transport为RoundTripperWrapper，则使用包装的RoundTripper获得dialFunc
	// 其他情况返回错误
	dialer, err := utilnet.DialerFor(transport)
	if err != nil {
		klog.V(5).Infof("Unable to unwrap transport %T to get dialer: %v", transport, err)
	}

	switch url.Scheme {
	case "http":
		if dialer != nil {
			return dialer(ctx, "tcp", dialAddr)
		}
		var d net.Dialer
		// dialer为nil，则执行net.Dialer的DialContext
		return d.DialContext(ctx, "tcp", dialAddr)
	case "https":
		// Get the tls config from the transport if we recognize it
		var tlsConfig *tls.Config
		var tlsConn *tls.Conn
		var err error
		// transport为nil，则返回nil
		// transport为http.Transport，则返回它的TLSClientConfig字段
		// transport为TLSClientConfigHolder，则执行TLSClientConfig()获得tls.Config
		// transport为RoundTripperWrapper，先执行transport.WrappedRoundTripper()获得真的transport，再次执行TLSClientConfig获得真实的tls.Config
		// 其他情况返回错误
		tlsConfig, err = utilnet.TLSClientConfig(transport)
		if err != nil {
			klog.V(5).Infof("Unable to unwrap transport %T to get at TLS config: %v", transport, err)
		}

		// 有dialer，则使用dialer生成conn，再利用这个conn进行tls连接
		if dialer != nil {
			// We have a dialer; use it to open the connection, then
			// create a tls client using the connection.
			netConn, err := dialer(ctx, "tcp", dialAddr)
			if err != nil {
				return nil, err
			}
			if tlsConfig == nil {
				// tls.Client requires non-nil config
				klog.Warningf("using custom dialer with no TLSClientConfig. Defaulting to InsecureSkipVerify")
				// tls.Handshake() requires ServerName or InsecureSkipVerify
				tlsConfig = &tls.Config{
					InsecureSkipVerify: true,
				}
			// tlsConfig.InsecureSkipVerify为false，且tlsConfig.ServerName为空，则从dialAddr解析出ip或host部分作为tlsConfig.ServerName
			} else if len(tlsConfig.ServerName) == 0 && !tlsConfig.InsecureSkipVerify {
				// tls.Handshake() requires ServerName or InsecureSkipVerify
				// infer the ServerName from the hostname we're connecting to.
				inferredHost := dialAddr
				// 解析出host和端口成功，则inferredHost为host
				if host, _, err := net.SplitHostPort(dialAddr); err == nil {
					inferredHost = host
				}
				// Make a copy to avoid polluting the provided config
				tlsConfigCopy := tlsConfig.Clone()
				tlsConfigCopy.ServerName = inferredHost
				tlsConfig = tlsConfigCopy
			}

			// Since this method is primary used within a "Connection: Upgrade" call we assume the caller is
			// going to write HTTP/1.1 request to the wire. http2 should not be allowed in the TLSConfig.NextProtos,
			// so we explicitly set that here. We only do this check if the TLSConfig support http/1.1.
			// tlsConfig.nextProtos为空，或包含"http/1.1"，则返回true
			// 否则返回false
			if supportsHTTP11(tlsConfig.NextProtos) {
				tlsConfig = tlsConfig.Clone()
				tlsConfig.NextProtos = []string{"http/1.1"}
			}

			tlsConn = tls.Client(netConn, tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				netConn.Close()
				return nil, err
			}

		} else {
			// Dial. This Dial method does not allow to pass a context unfortunately
			// 没有dialer，则直接使用tls.Dial进行连接
			tlsConn, err = tls.Dial("tcp", dialAddr, tlsConfig)
			if err != nil {
				return nil, err
			}
		}

		// Return if we were configured to skip validation
		// 有tlsConfig且不需要验证证书，则直接返回
		if tlsConfig != nil && tlsConfig.InsecureSkipVerify {
			return tlsConn, nil
		}

		// Verify
		// 验证证书和serverName
		host, _, _ := net.SplitHostPort(dialAddr)
		if tlsConfig != nil && len(tlsConfig.ServerName) > 0 {
			host = tlsConfig.ServerName
		}
		if err := tlsConn.VerifyHostname(host); err != nil {
			tlsConn.Close()
			return nil, err
		}

		return tlsConn, nil
	default:
		return nil, fmt.Errorf("Unknown scheme: %s", url.Scheme)
	}
}

// nextProtos为空，或包含"http/1.1"，则返回true
// 否则返回false
func supportsHTTP11(nextProtos []string) bool {
	if len(nextProtos) == 0 {
		return true
	}
	for _, proto := range nextProtos {
		if proto == "http/1.1" {
			return true
		}
	}
	return false
}
