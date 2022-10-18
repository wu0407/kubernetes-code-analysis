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

package websocket

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/util/wsstream"
)

const bearerProtocolPrefix = "base64url.bearer.authorization.k8s.io."

// 输出Sec-WebSocket-Protocol
var protocolHeader = textproto.CanonicalMIMEHeaderKey("Sec-WebSocket-Protocol")

var errInvalidToken = errors.New("invalid bearer token")

// ProtocolAuthenticator allows a websocket connection to provide a bearer token as a subprotocol
// in the format "base64url.bearer.authorization.<base64url-without-padding(bearer-token)>"
type ProtocolAuthenticator struct {
	// auth is the token authenticator to use to validate the token
	auth authenticator.Token
}

func NewProtocolAuthenticator(auth authenticator.Token) *ProtocolAuthenticator {
	return &ProtocolAuthenticator{auth}
}

// 从websocket里的header里"Sec-WebSocket-Protocol"的值，进行逗号分割，解析出"base64url.bearer.authorization.k8s.io."前缀的值
// 再对这个值的后缀中进行url解码，解析出token
// 降级到真正的authenticator.Token进行认证
func (a *ProtocolAuthenticator) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	// Only accept websocket connections
	// 不是websocket，直接返回nil, false, nil
	if !wsstream.IsWebSocketRequest(req) {
		return nil, false, nil
	}

	token := ""
	sawTokenProtocol := false
	filteredProtocols := []string{}
	// 遍历header里"Sec-WebSocket-Protocol"的值
	for _, protocolHeader := range req.Header[protocolHeader] {
		// 根据逗号进行分割
		for _, protocol := range strings.Split(protocolHeader, ",") {
			protocol = strings.TrimSpace(protocol)

			// protocol没有"base64url.bearer.authorization.k8s.io."前缀，则添加这个protocol到filteredProtocols
			if !strings.HasPrefix(protocol, bearerProtocolPrefix) {
				filteredProtocols = append(filteredProtocols, protocol)
				continue
			}

			if sawTokenProtocol {
				return nil, false, errors.New("multiple base64.bearer.authorization tokens specified")
			}
			sawTokenProtocol = true

			// protocol移除前缀"base64url.bearer.authorization.k8s.io."
			encodedToken := strings.TrimPrefix(protocol, bearerProtocolPrefix)
			// 使用url解码出token
			decodedToken, err := base64.RawURLEncoding.DecodeString(encodedToken)
			if err != nil {
				return nil, false, errors.New("invalid base64.bearer.authorization token encoding")
			}
			// token里有非utf8编码字符，则返回错误
			if !utf8.Valid(decodedToken) {
				return nil, false, errors.New("invalid base64.bearer.authorization token")
			}
			token = string(decodedToken)
		}
	}

	// Must pass at least one other subprotocol so that we can remove the one containing the bearer token,
	// and there is at least one to echo back to the client
	// 找到token，且不存在protocol没有"base64url.bearer.authorization.k8s.io."前缀，则返回错误
	if len(token) > 0 && len(filteredProtocols) == 0 {
		return nil, false, errors.New("missing additional subprotocol")
	}

	// 没有找到token，直接返回
	if len(token) == 0 {
		return nil, false, nil
	}

	// 降级到真正的authenticator.Token进行认证
	// 如果是staging\src\k8s.io\apiserver\pkg\authentication\token\cache\cached_token_authenticator.go里的cachedTokenAuthenticator
	// 先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
	resp, ok, err := a.auth.AuthenticateToken(req.Context(), token)

	// on success, remove the protocol with the token
	// 认证成功，则header "Sec-WebSocket-Protocol"只保留protocol没有"base64url.bearer.authorization.k8s.io."前缀
	if ok {
		// https://tools.ietf.org/html/rfc6455#section-11.3.4 indicates the Sec-WebSocket-Protocol header may appear multiple times
		// in a request, and is logically the same as a single Sec-WebSocket-Protocol header field that contains all values
		req.Header.Set(protocolHeader, strings.Join(filteredProtocols, ","))
	}

	// If the token authenticator didn't error, provide a default error
	if !ok && err == nil {
		err = errInvalidToken
	}

	return resp, ok, err
}
