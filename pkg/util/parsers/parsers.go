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

package parsers

import (
	"fmt"
	//  Import the crypto sha256 algorithm for the docker image parser to work
	_ "crypto/sha256"
	//  Import the crypto/sha512 algorithm for the docker image parser to work with 384 and 512 sha hashes
	_ "crypto/sha512"

	dockerref "github.com/docker/distribution/reference"
)

const (
	// DefaultImageTag is the default tag for docker image.
	DefaultImageTag = "latest"
)

// ParseImageName parses a docker image string into three parts: repo, tag and digest.
// If both tag and digest are empty, a default image tag will be returned.
// 从镜像地址解析出仓库地址、镜像tag、镜像digest
func ParseImageName(image string) (string, string, string, error) {
	// 解析镜像地址返回相应的Reference对象
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		return "", "", "", fmt.Errorf("couldn't parse image name: %v", err)
	}

	// 当named.domain为空，则返回named.path
	// 否则返回named.domain + "/" + named.path
	// 即返回仓库地址
	repoToPull := named.Name()
	var tag, digest string

	// 镜像地址是否包含tag
	tagged, ok := named.(dockerref.Tagged)
	// 镜像地址包含tag
	if ok {
		tag = tagged.Tag()
	}

	// 镜像地址是否包含digest
	digested, ok := named.(dockerref.Digested)
	// 镜像地址包含digest
	if ok {
		digest = digested.Digest().String()
	}
	// If no tag was specified, use the default "latest".
	// 处理tag为空且digest为空的情况
	// 如果都为空，则tag为"latest"
	if len(tag) == 0 && len(digest) == 0 {
		tag = DefaultImageTag
	}
	return repoToPull, tag, digest, nil
}
