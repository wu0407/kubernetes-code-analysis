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

package app

import (
	"errors"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	clientset "k8s.io/client-go/kubernetes"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"

	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/server"
)

// BuildAuth creates an authenticator, an authorizer, and a matching authorizer attributes getter compatible with the kubelet's needs
// It returns AuthInterface, a run method to start internal controllers (like cert reloading) and error.
func BuildAuth(nodeName types.NodeName, client clientset.Interface, config kubeletconfig.KubeletConfiguration) (server.AuthInterface, func(<-chan struct{}), error) {
	// Get clients, if provided
	var (
		tokenClient authenticationclient.TokenReviewInterface
		sarClient   authorizationclient.SubjectAccessReviewInterface
	)
	if client != nil && !reflect.ValueOf(client).IsNil() {
		tokenClient = client.AuthenticationV1().TokenReviews()
		sarClient = client.AuthorizationV1().SubjectAccessReviews()
	}

	// authenticator为authenticator.Request
	// runAuthenticatorCAReload为每一分钟读取config.Authentication.X509.ClientCAFile文件
	authenticator, runAuthenticatorCAReload, err := BuildAuthn(tokenClient, config.Authentication)
	if err != nil {
		return nil, nil, err
	}

	// 创建pkg\kubelet\server\auth.go里的nodeAuthorizerAttributesGetter{nodeName: nodeName}
	attributes := server.NewNodeAuthorizerAttributesGetter(nodeName)


	// 返回authorizer.Authorizer
	authorizer, err := BuildAuthz(sarClient, config.Authorization)
	if err != nil {
		return nil, nil, err
	}

	// 返回pkg\kubelet\server\auth.go里的KubeletAuth{authenticator, authorizerAttributeGetter, authorizer}
	return server.NewKubeletAuth(authenticator, attributes, authorizer), runAuthenticatorCAReload, nil
}

// BuildAuthn creates an authenticator compatible with the kubelet's needs
func BuildAuthn(client authenticationclient.TokenReviewInterface, authn kubeletconfig.KubeletAuthentication) (authenticator.Request, func(<-chan struct{}), error) {
	var dynamicCAContentFromFile *dynamiccertificates.DynamicFileCAContent
	var err error
	if len(authn.X509.ClientCAFile) > 0 {
		// 读取authn.X509.ClientCAFile文件，生成dynamicCAContentFromFile
		dynamicCAContentFromFile, err = dynamiccertificates.NewDynamicCAContentFromFile("client-ca-bundle", authn.X509.ClientCAFile)
		if err != nil {
			return nil, nil, err
		}
	}

	authenticatorConfig := authenticatorfactory.DelegatingAuthenticatorConfig{
		Anonymous:                          authn.Anonymous.Enabled,
		CacheTTL:                           authn.Webhook.CacheTTL.Duration,
		ClientCertificateCAContentProvider: dynamicCAContentFromFile,
	}

	if authn.Webhook.Enabled {
		if client == nil {
			return nil, nil, errors.New("no client provided, cannot use webhook authentication")
		}
		authenticatorConfig.TokenAccessReviewClient = client
	}

	// 根据authenticatorConfig创建聚合的authenticator.Request
	authenticator, _, err := authenticatorConfig.New()
	if err != nil {
		return nil, nil, err
	}

	// 每一分钟重新读取一下ca文件
	return authenticator, func(stopCh <-chan struct{}) {
		if dynamicCAContentFromFile != nil {
			go dynamicCAContentFromFile.Run(1, stopCh)
		}
	}, err
}

// BuildAuthz creates an authorizer compatible with the kubelet's needs
func BuildAuthz(client authorizationclient.SubjectAccessReviewInterface, authz kubeletconfig.KubeletAuthorization) (authorizer.Authorizer, error) {
	switch authz.Mode {
	// 如果授权模式是"AlwaysAllow"
	case kubeletconfig.KubeletAuthorizationModeAlwaysAllow:
		// 返回staging\src\k8s.io\apiserver\pkg\authorization\authorizerfactory\builtin.go里的alwaysAllowAuthorizer{}
		return authorizerfactory.NewAlwaysAllowAuthorizer(), nil

	// 如果授权模式是"Webhook"
	case kubeletconfig.KubeletAuthorizationModeWebhook:
		if client == nil {
			return nil, errors.New("no client provided, cannot use webhook authorization")
		}
		authorizerConfig := authorizerfactory.DelegatingAuthorizerConfig{
			SubjectAccessReviewClient: client,
			AllowCacheTTL:             authz.Webhook.CacheAuthorizedTTL.Duration,
			DenyCacheTTL:              authz.Webhook.CacheUnauthorizedTTL.Duration,
		}

		// 返回staging\src\k8s.io\apiserver\plugin\pkg\authorizer\webhook\webhook.go里的WebhookAuthorizer
		return authorizerConfig.New()

	case "":
		return nil, fmt.Errorf("no authorization mode specified")

	default:
		return nil, fmt.Errorf("unknown authorization mode %s", authz.Mode)

	}
}
