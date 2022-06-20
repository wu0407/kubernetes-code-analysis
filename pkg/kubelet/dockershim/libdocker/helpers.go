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

package libdocker

import (
	"strings"
	"time"

	dockerref "github.com/docker/distribution/reference"
	dockertypes "github.com/docker/docker/api/types"
	godigest "github.com/opencontainers/go-digest"
	"k8s.io/klog"
)

// ParseDockerTimestamp parses the timestamp returned by Interface from string to time.Time
func ParseDockerTimestamp(s string) (time.Time, error) {
	// Timestamp returned by Docker is in time.RFC3339Nano format.
	return time.Parse(time.RFC3339Nano, s)
}

// matchImageTagOrSHA checks if the given image specifier is a valid image ref,
// and that it matches the given image. It should fail on things like image IDs
// (config digests) and other digest-only references, but succeed on image names
// (`foo`), tag references (`foo:bar`), and manifest digest references
// (`foo@sha256:xyz`).
// 比较镜像inspect结果和镜像地址的tag、digest是否一致
func matchImageTagOrSHA(inspected dockertypes.ImageInspect, image string) bool {
	// The image string follows the grammar specified here
	// https://github.com/docker/distribution/blob/master/reference/reference.go#L4
	// 验证并解析镜像地址为Named对象
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		klog.V(4).Infof("couldn't parse image reference %q: %v", image, err)
		return false
	}
	// image地址是否包含tag
	_, isTagged := named.(dockerref.Tagged)
	// image地址是否包含digest
	digest, isDigested := named.(dockerref.Digested)
	// 即没有tag也没有digest，直接返回true（不需要比较）
	if !isTagged && !isDigested {
		// No Tag or SHA specified, so just return what we have
		return true
	}

	// 如果镜像地址包含tag
	if isTagged {
		// Check the RepoTags for a match.
		// 检查tag是否匹配
		for _, tag := range inspected.RepoTags {
			// An image name (without the tag/digest) can be [hostname '/'] component ['/' component]*
			// Because either the RepoTag or the name *may* contain the
			// hostname or not, we only check for the suffix match.
			// 比如RepoTags "mysql:latest"，image为"docker.io/mysql:latest"
			// 如果tag匹配镜像地址后缀或镜像匹配tag后缀，则直接返回true
			if strings.HasSuffix(image, tag) || strings.HasSuffix(tag, image) {
				return true
			} else {
				// TODO: We need to remove this hack when project atomic based
				// docker distro(s) like centos/fedora/rhel image fix problems on
				// their end.
				// Say the tag is "docker.io/busybox:latest"
				// and the image is "docker.io/library/busybox:latest"
				// 验证并解析镜像地址为Named对象
				t, err := dockerref.ParseNormalizedNamed(tag)
				if err != nil {
					continue
				}
				// the parsed/normalized tag will look like
				// reference.taggedReference {
				// 	 namedRepository: reference.repository {
				// 	   domain: "docker.io",
				// 	   path: "library/busybox"
				//	},
				// 	tag: "latest"
				// }
				// If it does not have tags then we bail out
				t2, ok := t.(dockerref.Tagged)
				if !ok {
					continue
				}
				// normalized tag would look like "docker.io/library/busybox:latest"
				// note the library get added in the string
				// 返回t2.domain + "/" + t2.path + ":" + t2.tag
				normalizedTag := t2.String()
				// normalizedTag为空字符串（不会出现这种情况），则跳过继续下一个tag
				if normalizedTag == "" {
					continue
				}
				if strings.HasSuffix(image, normalizedTag) || strings.HasSuffix(normalizedTag, image) {
					return true
				}
			}
		}
	}

	// 如果镜像地址包含digest
	if isDigested {
		for _, repoDigest := range inspected.RepoDigests {
			// RepoDigests 比如"mysql@sha256:aeecae58035f3868bf4f00e5fc623630d8b438db9d05f4d8c6538deb14d4c31b"
			// 验证并解析镜像地址为Named对象
			named, err := dockerref.ParseNormalizedNamed(repoDigest)
			if err != nil {
				klog.V(4).Infof("couldn't parse image RepoDigest reference %q: %v", repoDigest, err)
				continue
			}
			// 镜像地址解析出来的digest与inspect结果里的repoDigest，比较算法和hash值是否一样
			if d, isDigested := named.(dockerref.Digested); isDigested {
				if digest.Digest().Algorithm().String() == d.Digest().Algorithm().String() &&
					digest.Digest().Hex() == d.Digest().Hex() {
					return true
				}
			}
		}

		// id做为digest情况，inspected.ID类似这样 "sha256:b05128b000ddbafb0a0d2713086c6a1cc23280dee3529d37f03c98c97c8cf1ed"

		// process the ID as a digest
		id, err := godigest.Parse(inspected.ID)
		if err != nil {
			klog.V(4).Infof("couldn't parse image ID reference %q: %v", id, err)
			return false
		}
		// 镜像地址解析出来的digest与镜像id，比较算法和hash值是否一样
		if digest.Digest().Algorithm().String() == id.Algorithm().String() && digest.Digest().Hex() == id.Hex() {
			return true
		}
	}
	klog.V(4).Infof("Inspected image (%q) does not match %s", inspected.ID, image)
	return false
}

// matchImageIDOnly checks that the given image specifier is a digest-only
// reference, and that it matches the given image.
// 检查image与inspect结果里id一样，或者是否image里包含digest且digest为inspect结果的id
func matchImageIDOnly(inspected dockertypes.ImageInspect, image string) bool {
	// If the image ref is literally equal to the inspected image's ID,
	// just return true here (this might be the case for Docker 1.9,
	// where we won't have a digest for the ID)
	if inspected.ID == image {
		return true
	}

	// Otherwise, we should try actual parsing to be more correct
	// image不匹配inspect结果里镜像id，则解析image返回相应的Reference对象
	ref, err := dockerref.Parse(image)
	if err != nil {
		klog.V(4).Infof("couldn't parse image reference %q: %v", image, err)
		return false
	}

	// 检测image里是否包含digest
	digest, isDigested := ref.(dockerref.Digested)
	// 没有包含digest，直接返回错误
	if !isDigested {
		klog.V(4).Infof("the image reference %q was not a digest reference", image)
		return false
	}

	// image里包含digest，判断是否是id作为digest情况

	id, err := godigest.Parse(inspected.ID)
	if err != nil {
		klog.V(4).Infof("couldn't parse image ID reference %q: %v", id, err)
		return false
	}

	// image解析出来的digest与inspect结果的id，比较算法和hash值是否一样
	if digest.Digest().Algorithm().String() == id.Algorithm().String() && digest.Digest().Hex() == id.Hex() {
		return true
	}

	klog.V(4).Infof("The reference %s does not directly refer to the given image's ID (%q)", image, inspected.ID)
	return false
}
