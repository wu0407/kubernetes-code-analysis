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

package cache

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sync/singleflight"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilclock "k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/klog"
)

var errAuthnCrash = apierrors.NewInternalError(errors.New("authentication failed unexpectedly"))

const sharedLookupTimeout = 30 * time.Second

// cacheRecord holds the three return values of the authenticator.Token AuthenticateToken method
type cacheRecord struct {
	resp *authenticator.Response
	ok   bool
	err  error
}

type cachedTokenAuthenticator struct {
	authenticator authenticator.Token

	cacheErrs  bool
	successTTL time.Duration
	failureTTL time.Duration

	cache cache
	group singleflight.Group

	// hashPool is a per authenticator pool of hash.Hash (to avoid allocations from building the Hash)
	// HMAC with SHA-256 and a random key is used to prevent precomputation and length extension attacks
	// It also mitigates hash map DOS attacks via collisions (the inputs are supplied by untrusted users)
	hashPool *sync.Pool
}

type cache interface {
	// given a key, return the record, and whether or not it existed
	get(key string) (value *cacheRecord, exists bool)
	// caches the record for the key
	set(key string, value *cacheRecord, ttl time.Duration)
	// removes the record for the key
	remove(key string)
}

// New returns a token authenticator that caches the results of the specified authenticator. A ttl of 0 bypasses the cache.
func New(authenticator authenticator.Token, cacheErrs bool, successTTL, failureTTL time.Duration) authenticator.Token {
	return newWithClock(authenticator, cacheErrs, successTTL, failureTTL, utilclock.RealClock{})
}

func newWithClock(authenticator authenticator.Token, cacheErrs bool, successTTL, failureTTL time.Duration, clock utilclock.Clock) authenticator.Token {
	randomCacheKey := make([]byte, 32)
	if _, err := rand.Read(randomCacheKey); err != nil {
		panic(err) // rand should never fail
	}

	return &cachedTokenAuthenticator{
		authenticator: authenticator,
		cacheErrs:     cacheErrs,
		successTTL:    successTTL,
		failureTTL:    failureTTL,
		// Cache performance degrades noticeably when the number of
		// tokens in operation exceeds the size of the cache. It is
		// cheap to make the cache big in the second dimension below,
		// the memory is only consumed when that many tokens are being
		// used. Currently we advertise support 5k nodes and 10k
		// namespaces; a 32k entry cache is therefore a 2x safety
		// margin.
		// cache第一层为stripedCache（32个simpleCache），第二层是simpleCache
		// 第三层为staging\src\k8s.io\apimachinery\pkg\util\cache\expiring.go的Expiring
		cache: newStripedCache(32, fnvHashFunc, func() cache { return newSimpleCache(clock) }),

		hashPool: &sync.Pool{
			New: func() interface{} {
				return hmac.New(sha256.New, randomCacheKey)
			},
		},
	}
}

// AuthenticateToken implements authenticator.Token
// 先通过缓存中查找未过期的认证响应，未找到则进行请求apiserver，进行token认证（一个token同一时时间只有一个请求）
func (a *cachedTokenAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	// 统计命中监控信息的函数，记录metrics
	doneAuthenticating := stats.authenticating()

	auds, audsOk := authenticator.AudiencesFrom(ctx)

	// 生成一个key（从a.hashPool区一个hash，根据auds, token进行hash生成string）
	key := keyFunc(a.hashPool, auds, token)
	// 缓存中有record，直接返回缓存中的响应
	if record, ok := a.cache.get(key); ok {
		// Record cache hit
		doneAuthenticating(true)
		return record.resp, record.ok, record.err
	}

	// Record cache miss
	doneBlocking := stats.blocking()
	defer doneBlocking()
	defer doneAuthenticating(false)

	type lookup struct {
		resp *authenticator.Response
		ok   bool
	}

	c := a.group.DoChan(key, func() (val interface{}, err error) {
		doneFetching := stats.fetching()
		// We're leaving the request handling stack so we need to handle crashes
		// ourselves. Log a stack trace and return a 500 if something panics.
		defer func() {
			if r := recover(); r != nil {
				err = errAuthnCrash
				// Same as stdlib http server code. Manually allocate stack
				// trace buffer size to prevent excessively large logs
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]
				klog.Errorf("%v\n%s", r, buf)
			}
			doneFetching(err == nil)
		}()

		// Check again for a cached record. We may have raced with a fetch.
		if record, ok := a.cache.get(key); ok {
			return lookup{record.resp, record.ok}, record.err
		}

		// Detach the context because the lookup may be shared by multiple callers,
		// however propagate the audience.
		ctx, cancel := context.WithTimeout(context.Background(), sharedLookupTimeout)
		defer cancel()

		// 如果request里包含audience，则将audience重新包进context
		if audsOk {
			ctx = authenticator.WithAudiences(ctx, auds)
		}

		// 降级到真正的authenticator进行认证
		// 如果staging\src\k8s.io\apiserver\plugin\pkg\authenticator\token\webhook\webhook.go里的WebhookTokenAuthenticator
		// 请求apiserver根据token创建tokenReview，进行认证
		resp, ok, err := a.authenticator.AuthenticateToken(ctx, token)
		// 未启用缓存错误，且err不为nil，则直接返回nil，错误
		if !a.cacheErrs && err != nil {
			return nil, err
		}

		switch {
		// 认证成功，且配置了缓存成功时间，则将认证响应保存到缓存中
		case ok && a.successTTL > 0:
			a.cache.set(key, &cacheRecord{resp: resp, ok: ok, err: err}, a.successTTL)
		// 认证不成功，且配置了缓存不成功时间，则将认证响应保存到缓存中
		case !ok && a.failureTTL > 0:
			a.cache.set(key, &cacheRecord{resp: resp, ok: ok, err: err}, a.failureTTL)
		}
		return lookup{resp, ok}, err
	})

	select {
	case result := <-c:
		if result.Err != nil {
			return nil, false, result.Err
		}
		lookup := result.Val.(lookup)
		return lookup.resp, lookup.ok, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// keyFunc generates a string key by hashing the inputs.
// This lowers the memory requirement of the cache and keeps tokens out of memory.
func keyFunc(hashPool *sync.Pool, auds []string, token string) string {
	h := hashPool.Get().(hash.Hash)

	h.Reset()

	// try to force stack allocation
	var a [4]byte
	// b的长度为4
	b := a[:]

	// 将token的长度值以uint32形式保存到b中，并将b写入到h
	// 将token转成byte写入到h
	writeLengthPrefixedString(h, b, token)
	// encode the length of audiences to avoid ambiguities
	// 将auds的长度值以uint32形式保存到b中，并将b写入到h
	writeLength(h, b, len(auds))
	for _, aud := range auds {
		// 将aud的长度值以uint32形式保存到b中，并将b写入到h
		// 将aud转成byte写入到h
		writeLengthPrefixedString(h, b, aud)
	}

	// h里保存了：{token长度}{token}{auds长度}{aud1长度}{aud1}{aud2长度}{aud2}...

	key := toString(h.Sum(nil)) // skip base64 encoding to save an allocation

	hashPool.Put(h)

	return key
}

// writeLengthPrefixedString writes s with a length prefix to prevent ambiguities, i.e. "xy" + "z" == "x" + "yz"
// the length of b is assumed to be 4 (b is mutated by this function to store the length of s)
func writeLengthPrefixedString(w io.Writer, b []byte, s string) {
	// 将s的长度值以uint32形式保存到b中，并将b写入到w
	writeLength(w, b, len(s))
	// 将s转成byte写入到w
	if _, err := w.Write(toBytes(s)); err != nil {
		panic(err) // Write() on hash never fails
	}
}

// writeLength encodes length into b and then writes it via the given writer
// the length of b is assumed to be 4
func writeLength(w io.Writer, b []byte, length int) {
	// 将length以uint32形式保存到b中
	binary.BigEndian.PutUint32(b, uint32(length))
	// 将b写入到w中
	if _, err := w.Write(b); err != nil {
		panic(err) // Write() on hash never fails
	}
}

// toBytes performs unholy acts to avoid allocations
func toBytes(s string) []byte {
	return *(*[]byte)(unsafe.Pointer(&s))
}

// toString performs unholy acts to avoid allocations
func toString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}
