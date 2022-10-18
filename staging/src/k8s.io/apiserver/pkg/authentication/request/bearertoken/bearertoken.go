/*
Copyright 2014 The Kubernetes Authors.

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

package bearertoken

import (
	"errors"
	"net/http"
	"strings"

	"k8s.io/apiserver/pkg/authentication/authenticator"
)

type Authenticator struct {
	auth authenticator.Token
}

func New(auth authenticator.Token) *Authenticator {
	return &Authenticator{auth}
}

var invalidToken = errors.New("invalid bearer token")

// 先从header中解析出"Authorization"的header值，在从这个header值中解析出token
// 先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
func (a *Authenticator) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	// 从request header头部获得"Authorization"的header值
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	// "Authorization"的header值为空
	if auth == "" {
		return nil, false, nil
	}
	parts := strings.Split(auth, " ")
	// "Authorization"的header值不包含两个字段，或第一个字段不为"bearer"
	if len(parts) < 2 || strings.ToLower(parts[0]) != "bearer" {
		return nil, false, nil
	}

	token := parts[1]

	// Empty bearer tokens aren't valid
	if len(token) == 0 {
		return nil, false, nil
	}

	// 先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
	resp, ok, err := a.auth.AuthenticateToken(req.Context(), token)
	// if we authenticated successfully, go ahead and remove the bearer token so that no one
	// is ever tempted to use it inside of the API server
	// 认证成功则移除请求头部的"Authorization"
	if ok {
		req.Header.Del("Authorization")
	}

	// If the token authenticator didn't error, provide a default error
	// 未发生错误，且认证不成功，则错误为"invalid bearer token"
	if !ok && err == nil {
		err = invalidToken
	}

	return resp, ok, err
}
