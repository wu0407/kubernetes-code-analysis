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

package authenticatorfactory

import (
	"errors"
	"time"

	"github.com/go-openapi/spec"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/group"
	"k8s.io/apiserver/pkg/authentication/request/anonymous"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	unionauth "k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/request/websocket"
	"k8s.io/apiserver/pkg/authentication/request/x509"
	"k8s.io/apiserver/pkg/authentication/token/cache"
	webhooktoken "k8s.io/apiserver/plugin/pkg/authenticator/token/webhook"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// DelegatingAuthenticatorConfig is the minimal configuration needed to create an authenticator
// built to delegate authentication to a kube API server
type DelegatingAuthenticatorConfig struct {
	Anonymous bool

	// TokenAccessReviewClient is a client to do token review. It can be nil. Then every token is ignored.
	TokenAccessReviewClient authenticationclient.TokenReviewInterface

	// CacheTTL is the length of time that a token authentication answer will be cached.
	CacheTTL time.Duration

	// CAContentProvider are the options for verifying incoming connections using mTLS and directly assigning to users.
	// Generally this is the CA bundle file used to authenticate client certificates
	// If this is nil, then mTLS will not be used.
	ClientCertificateCAContentProvider CAContentProvider

	APIAudiences authenticator.Audiences

	RequestHeaderConfig *RequestHeaderConfig
}

// 生成聚合的authenticator.Request
// 其中根据header来进行认证，包装了requestHeaderAuthRequestHandler，先进行证书验证，然后进行header认证
// 其中根据证书进行认证，先验证客户端提供的证书，然后执行a.user从证书中提取认证信息
// 其中根据bearer token进行认证
//   先从header中解析出"Authorization"的header值，在从这个header值中解析出token
//   先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
// 其中websocket里根据bearer token进行认证
//   从websocket里的header里"Sec-WebSocket-Protocol"的值，进行逗号分割，解析出"base64url.bearer.authorization.k8s.io."前缀的值
//   再对这个值的后缀中进行url解码，解析出token
//   降级到真正的authenticator.Token进行认证（先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
func (c DelegatingAuthenticatorConfig) New() (authenticator.Request, *spec.SecurityDefinitions, error) {
	authenticators := []authenticator.Request{}
	securityDefinitions := spec.SecurityDefinitions{}

	// front-proxy first, then remote
	// Add the front proxy authenticator if requested
	if c.RequestHeaderConfig != nil {
		// 生成staging\src\k8s.io\apiserver\pkg\authentication\request\x509\x509.go里的Verifier
		requestHeaderAuthenticator := headerrequest.NewDynamicVerifyOptionsSecure(
			c.RequestHeaderConfig.CAContentProvider.VerifyOptions,
			c.RequestHeaderConfig.AllowedClientNames,
			c.RequestHeaderConfig.UsernameHeaders,
			c.RequestHeaderConfig.GroupHeaders,
			c.RequestHeaderConfig.ExtraHeaderPrefixes,
		)
		authenticators = append(authenticators, requestHeaderAuthenticator)
	}

	// x509 client cert auth
	if c.ClientCertificateCAContentProvider != nil {
		// 生成staging\src\k8s.io\apiserver\pkg\authentication\request\x509\x509.go里的Authenticator
		authenticators = append(authenticators, x509.NewDynamic(c.ClientCertificateCAContentProvider.VerifyOptions, x509.CommonNameUserConversion))
	}

	if c.TokenAccessReviewClient != nil {
		// 生成staging\src\k8s.io\apiserver\plugin\pkg\authenticator\token\webhook\webhook.go里的WebhookTokenAuthenticator
		tokenAuth, err := webhooktoken.NewFromInterface(c.TokenAccessReviewClient, c.APIAudiences)
		if err != nil {
			return nil, nil, err
		}
		// 生成staging\src\k8s.io\apiserver\pkg\authentication\token\cache\cached_token_authenticator.go里的cachedTokenAuthenticator
		// 包装了tokenAuth
		cachingTokenAuth := cache.New(tokenAuth, false, c.CacheTTL, c.CacheTTL)
		// bearertoken.New
		// 生成staging\src\k8s.io\apiserver\pkg\authentication\request\bearertoken\bearertoken.go里的Authenticator
		// websocket.NewProtocolAuthenticator
		// 生成staging\src\k8s.io\apiserver\pkg\authentication\request\websocket\protocol.go里的ProtocolAuthenticator
		authenticators = append(authenticators, bearertoken.New(cachingTokenAuth), websocket.NewProtocolAuthenticator(cachingTokenAuth))

		securityDefinitions["BearerToken"] = &spec.SecurityScheme{
			SecuritySchemeProps: spec.SecuritySchemeProps{
				Type:        "apiKey",
				Name:        "authorization",
				In:          "header",
				Description: "Bearer Token authentication",
			},
		}
	}

	// 没有配置认证方法，判断是否启用anonymous
	if len(authenticators) == 0 {
		// 启用Anonymous
		if c.Anonymous {
			return anonymous.NewAuthenticator(), &securityDefinitions, nil
		}
		return nil, nil, errors.New("No authentication method configured")
	}

	// unionauth.New返回staging\src\k8s.io\apiserver\pkg\authentication\request\union\union.go
	// unionAuthRequestHandler{Handlers: authRequestHandlers, FailOnError: false}
	// 用来聚合所有的authRequestHandlers
	// 处理请求时，遍历所有的authRequestHandlers，执行AuthenticateRequest(req)，有一个handler执行成功就返回。
	// 
	// group.NewAuthenticatedGroupAdder返回staging\src\k8s.io\apiserver\pkg\authentication\group\authenticated_group_adder.go里的
	// AuthenticatedGroupAdder{auth}
	// 处理请求的时候，会对响应里用户所属组添加"system:authenticated"
	authenticator := group.NewAuthenticatedGroupAdder(unionauth.New(authenticators...))
	// 启用Anonymous
	if c.Anonymous {
		// 返回staging\src\k8s.io\apiserver\pkg\authentication\request\union\union.go
		// 如果是多个authenticator.Request，返回unionAuthRequestHandler{Handlers: authRequestHandlers, FailOnError: true}
		// 如果是一个authenticator.Request，就返回这个authenticator.Request
		// 
		// 这里返回unionAuthRequestHandler{Handlers: authRequestHandlers, FailOnError: true}
		authenticator = unionauth.NewFailOnError(authenticator, anonymous.NewAuthenticator())
	}
	return authenticator, &securityDefinitions, nil
}
