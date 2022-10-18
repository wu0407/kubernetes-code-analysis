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

package streaming

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
)

var (
	// cacheTTL is the timeout after which tokens become invalid.
	cacheTTL = 1 * time.Minute
	// maxInFlight is the maximum number of in-flight requests to allow.
	maxInFlight = 1000
	// tokenLen is the length of the random base64 encoded token identifying the request.
	tokenLen = 8
)

// requestCache caches streaming (exec/attach/port-forward) requests and generates a single-use
// random token for their retrieval. The requestCache is used for building streaming URLs without
// the need to encode every request parameter in the URL.
type requestCache struct {
	// clock is used to obtain the current time
	clock clock.Clock

	// tokens maps the generate token to the request for fast retrieval.
	tokens map[string]*list.Element
	// ll maintains an age-ordered request list for faster garbage collection of expired requests.
	ll *list.List

	lock sync.Mutex
}

// Type representing an *ExecRequest, *AttachRequest, or *PortForwardRequest.
type request interface{}

type cacheEntry struct {
	token      string
	req        request
	expireTime time.Time
}

func newRequestCache() *requestCache {
	return &requestCache{
		clock:  clock.RealClock{},
		ll:     list.New(),
		tokens: make(map[string]*list.Element),
	}
}

// Insert the given request into the cache and returns the token used for fetching it out.
// 先从c.ll（链表保存所有的cacheEntry）和c.tokens（hash值对应cacheEntry）移除过期元素
// 再向c.ll（链表保存所有的cacheEntry）添加到开头和c.tokens（hash值对应cacheEntry）添加元素
// 返回用来索引的token
func (c *requestCache) Insert(req request) (token string, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Remove expired entries.
	// 从c.ll（链表保存所有的cacheEntry）和c.tokens（hash值对应cacheEntry）移除过期元素
	c.gc()
	// If the cache is full, reject the request.
	if c.ll.Len() == maxInFlight {
		return "", NewErrorTooManyInFlight()
	}
	// 生成唯一的token
	token, err = c.uniqueToken()
	if err != nil {
		return "", err
	}
	ele := c.ll.PushFront(&cacheEntry{token, req, c.clock.Now().Add(cacheTTL)})

	c.tokens[token] = ele
	return token, nil
}

// Consume the token (remove it from the cache) and return the cached request, if found.
// 从c.tokens中获得cacheEntry
// token对应得请求不存在，则直接返回nil，false
// 如果存在，则从c.ll和c.tokens中移除，如果过期了，则返回nil，false
// 否则返回request
func (c *requestCache) Consume(token string) (req request, found bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	ele, ok := c.tokens[token]
	// token对应得请求不存在，则直接返回nil，false
	if !ok {
		return nil, false
	}
	// 从c.ll中移除这个list.Element
	c.ll.Remove(ele)
	// 从c.tokens中移除
	delete(c.tokens, token)

	entry := ele.Value.(*cacheEntry)
	// 如果过期了，则返回nil，false
	if c.clock.Now().After(entry.expireTime) {
		// Entry already expired.
		return nil, false
	}
	return entry.req, true
}

// uniqueToken generates a random URL-safe token and ensures uniqueness.
// 生成唯一的token
func (c *requestCache) uniqueToken() (string, error) {
	const maxTries = 10
	// Number of bytes to be tokenLen when base64 encoded.
	tokenSize := math.Ceil(float64(tokenLen) * 6 / 8)
	rawToken := make([]byte, int(tokenSize))
	for i := 0; i < maxTries; i++ {
		// 生成随机token
		if _, err := rand.Read(rawToken); err != nil {
			return "", err
		}
		// 对token进行url编码
		encoded := base64.RawURLEncoding.EncodeToString(rawToken)
		token := encoded[:tokenLen]
		// If it's unique, return it. Otherwise retry.
		if _, exists := c.tokens[encoded]; !exists {
			return token, nil
		}
	}
	return "", fmt.Errorf("failed to generate unique token")
}

// Must be write-locked prior to calling.
// 从c.ll（链表保存所有的cacheEntry）和c.tokens（hash值对应cacheEntry）移除过期元素
func (c *requestCache) gc() {
	now := c.clock.Now()
	for c.ll.Len() > 0 {
		oldest := c.ll.Back()
		entry := oldest.Value.(*cacheEntry)
		// 未过期直接返回
		if !now.After(entry.expireTime) {
			return
		}

		// Oldest value is expired; remove it.
		c.ll.Remove(oldest)
		// 从c.tokens移除
		delete(c.tokens, entry.token)
	}
}
