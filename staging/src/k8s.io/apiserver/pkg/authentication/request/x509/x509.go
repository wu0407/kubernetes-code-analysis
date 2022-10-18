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

package x509

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/http"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

/*
 * By default, the following metric is defined as falling under
 * ALPHA stability level https://github.com/kubernetes/enhancements/blob/master/keps/sig-instrumentation/20190404-kubernetes-control-plane-metrics-stability.md#stability-classes)
 *
 * Promoting the stability level of the metric is a responsibility of the component owner, since it
 * involves explicitly acknowledging support for the metric across multiple releases, in accordance with
 * the metric stability policy.
 */
var clientCertificateExpirationHistogram = metrics.NewHistogram(
	&metrics.HistogramOpts{
		Namespace: "apiserver",
		Subsystem: "client",
		Name:      "certificate_expiration_seconds",
		Help:      "Distribution of the remaining lifetime on the certificate used to authenticate a request.",
		Buckets: []float64{
			0,
			(30 * time.Minute).Seconds(),
			(1 * time.Hour).Seconds(),
			(2 * time.Hour).Seconds(),
			(6 * time.Hour).Seconds(),
			(12 * time.Hour).Seconds(),
			(24 * time.Hour).Seconds(),
			(2 * 24 * time.Hour).Seconds(),
			(4 * 24 * time.Hour).Seconds(),
			(7 * 24 * time.Hour).Seconds(),
			(30 * 24 * time.Hour).Seconds(),
			(3 * 30 * 24 * time.Hour).Seconds(),
			(6 * 30 * 24 * time.Hour).Seconds(),
			(12 * 30 * 24 * time.Hour).Seconds(),
		},
		StabilityLevel: metrics.ALPHA,
	},
)

func init() {
	legacyregistry.MustRegister(clientCertificateExpirationHistogram)
}

// UserConversion defines an interface for extracting user info from a client certificate chain
type UserConversion interface {
	User(chain []*x509.Certificate) (*authenticator.Response, bool, error)
}

// UserConversionFunc is a function that implements the UserConversion interface.
type UserConversionFunc func(chain []*x509.Certificate) (*authenticator.Response, bool, error)

// User implements x509.UserConversion
func (f UserConversionFunc) User(chain []*x509.Certificate) (*authenticator.Response, bool, error) {
	return f(chain)
}

// VerifyOptionFunc is function which provides a shallow copy of the VerifyOptions to the authenticator.  This allows
// for cases where the options (particularly the CAs) can change.  If the bool is false, then the returned VerifyOptions
// are ignored and the authenticator will express "no opinion".  This allows a clear signal for cases where a CertPool
// is eventually expected, but not currently present.
type VerifyOptionFunc func() (x509.VerifyOptions, bool)

// Authenticator implements request.Authenticator by extracting user info from verified client certificates
type Authenticator struct {
	verifyOptionsFn VerifyOptionFunc
	user            UserConversion
}

// New returns a request.Authenticator that verifies client certificates using the provided
// VerifyOptions, and converts valid certificate chains into user.Info using the provided UserConversion
func New(opts x509.VerifyOptions, user UserConversion) *Authenticator {
	return NewDynamic(StaticVerifierFn(opts), user)
}

// NewDynamic returns a request.Authenticator that verifies client certificates using the provided
// VerifyOptionFunc (which may be dynamic), and converts valid certificate chains into user.Info using the provided UserConversion
func NewDynamic(verifyOptionsFn VerifyOptionFunc, user UserConversion) *Authenticator {
	return &Authenticator{verifyOptionsFn, user}
}

// AuthenticateRequest authenticates the request using presented client certificates
// 先验证客户端提供的证书，然后执行a.user从证书中提取认证信息
// 如果a.user为CommonNameUserConversion
// 使用第一个证书里的subject's CommonName，进行认证
// 其中username为Subject.CommonName，group为Subject.Organization
func (a *Authenticator) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	// 不是tls连接或客户端没有设置对端证书PeerCertificates
	if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		return nil, false, nil
	}

	// Use intermediates, if provided
	optsCopy, ok := a.verifyOptionsFn()
	// if there are intentionally no verify options, then we cannot authenticate this request
	if !ok {
		return nil, false, nil
	}
	// 客户端提供了证书且optsCopy.Intermediates为nil（签发人证书链为空）
	if optsCopy.Intermediates == nil && len(req.TLS.PeerCertificates) > 1 {
		optsCopy.Intermediates = x509.NewCertPool()
		// 将客户端提供的第2个证书到最后一个证书，添加到optsCopy.Intermediates里
		for _, intermediate := range req.TLS.PeerCertificates[1:] {
			optsCopy.Intermediates.AddCert(intermediate)
		}
	}

	// 证书还剩的过期时间
	remaining := req.TLS.PeerCertificates[0].NotAfter.Sub(time.Now())
	clientCertificateExpirationHistogram.Observe(remaining.Seconds())
	// 验证客户端提供的证书链，并返回证书链（包括提供的和optsCopy.Roots）
	chains, err := req.TLS.PeerCertificates[0].Verify(optsCopy)
	if err != nil {
		return nil, false, err
	}

	var errlist []error
	for _, chain := range chains {
		// 如果a.user为CommonNameUserConversion
		// 使用第一个证书里的subject's CommonName，进行认证
		// 其中username为Subject.CommonName，group为Subject.Organization
		user, ok, err := a.user.User(chain)
		if err != nil {
			errlist = append(errlist, err)
			continue
		}

		// 有Subject.commonName，并解析出user
		if ok {
			return user, ok, err
		}
	}
	return nil, false, utilerrors.NewAggregate(errlist)
}

// Verifier implements request.Authenticator by verifying a client cert on the request, then delegating to the wrapped auth
type Verifier struct {
	verifyOptionsFn VerifyOptionFunc
	auth            authenticator.Request

	// allowedCommonNames contains the common names which a verified certificate is allowed to have.
	// If empty, all verified certificates are allowed.
	allowedCommonNames StringSliceProvider
}

// NewVerifier create a request.Authenticator by verifying a client cert on the request, then delegating to the wrapped auth
func NewVerifier(opts x509.VerifyOptions, auth authenticator.Request, allowedCommonNames sets.String) authenticator.Request {
	return NewDynamicCAVerifier(StaticVerifierFn(opts), auth, StaticStringSlice(allowedCommonNames.List()))
}

// NewDynamicCAVerifier create a request.Authenticator by verifying a client cert on the request, then delegating to the wrapped auth
func NewDynamicCAVerifier(verifyOptionsFn VerifyOptionFunc, auth authenticator.Request, allowedCommonNames StringSliceProvider) authenticator.Request {
	return &Verifier{verifyOptionsFn, auth, allowedCommonNames}
}

// AuthenticateRequest verifies the presented client certificate, then delegates to the wrapped auth
// 先进行客户端证书验证，然后降级到a.auth进行认证
func (a *Verifier) AuthenticateRequest(req *http.Request) (*authenticator.Response, bool, error) {
	// 不是tls连接或客户端没有设置对端证书PeerCertificates
	if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		return nil, false, nil
	}

	// Use intermediates, if provided
	optsCopy, ok := a.verifyOptionsFn()
	// if there are intentionally no verify options, then we cannot authenticate this request
	if !ok {
		return nil, false, nil
	}
	// 客户端提供了证书且optsCopy.Intermediates为nil（签发人证书链为空）
	if optsCopy.Intermediates == nil && len(req.TLS.PeerCertificates) > 1 {
		optsCopy.Intermediates = x509.NewCertPool()
		// 将客户端提供的第2个证书到最后一个证书，添加到optsCopy.Intermediates里
		for _, intermediate := range req.TLS.PeerCertificates[1:] {
			optsCopy.Intermediates.AddCert(intermediate)
		}
	}

	// 对客户端提供的证书（证书里的第一个证书）进行验证（使用客户端提供的证书链进行验证）
	if _, err := req.TLS.PeerCertificates[0].Verify(optsCopy); err != nil {
		return nil, false, err
	}
	// 验证第一个证书里的subject.CommonName是否在允许的CommonName集合里，不在集合里直接返回错误
	if err := a.verifySubject(req.TLS.PeerCertificates[0].Subject); err != nil {
		return nil, false, err
	}
	// 从request的header中解析出nameHeaders、groupHeader、extraHeaderPrefixes的header的值
	// 从req的header中移除a.nameHeaders、a.groupHeaders，并移除带有a.extraHeaderPrefixes前缀的header
	return a.auth.AuthenticateRequest(req)
}

// 验证subject.CommonName是否在允许的CommonName集合里，不在集合里直接返回错误
func (a *Verifier) verifySubject(subject pkix.Name) error {
	// No CN restrictions
	// 没有设置允许的CommonName，则直接返回
	if len(a.allowedCommonNames.Value()) == 0 {
		return nil
	}
	// Enforce CN restrictions
	// 证书里subject.CommonName在allowedCommonName（允许的CommonName）集合里，则返回
	for _, allowedCommonName := range a.allowedCommonNames.Value() {
		if allowedCommonName == subject.CommonName {
			return nil
		}
	}
	return fmt.Errorf("x509: subject with cn=%s is not in the allowed list", subject.CommonName)
}

// DefaultVerifyOptions returns VerifyOptions that use the system root certificates, current time,
// and requires certificates to be valid for client auth (x509.ExtKeyUsageClientAuth)
func DefaultVerifyOptions() x509.VerifyOptions {
	return x509.VerifyOptions{
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

// CommonNameUserConversion builds user info from a certificate chain using the subject's CommonName
// 返回function
// 使用第一个证书里的subject's CommonName，进行认证
// 其中username为Subject.CommonName，group为Subject.Organization
var CommonNameUserConversion = UserConversionFunc(func(chain []*x509.Certificate) (*authenticator.Response, bool, error) {
	if len(chain[0].Subject.CommonName) == 0 {
		return nil, false, nil
	}
	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   chain[0].Subject.CommonName,
			Groups: chain[0].Subject.Organization,
		},
	}, true, nil
})
